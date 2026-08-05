// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	extism "github.com/extism/go-sdk"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// invocation is one plugin call's authority and budget. It travels on the
// context because Extism binds host functions at compile time while tenant and
// plugin vary per call — the alternative is compiling a module per tenant.
type invocation struct {
	host   Host
	tenant tenancy.ID
	plugin *Installed

	mu     sync.Mutex
	budget int
	calls  int
	err    error // the first host-side refusal, surfaced instead of the WASM trap
}

type invocationKey struct{}

func withInvocation(ctx context.Context, inv *invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, inv)
}

func invocationFromContext(ctx context.Context) (*invocation, bool) {
	inv, ok := ctx.Value(invocationKey{}).(*invocation)
	return inv, ok
}

// spend consumes one host-call budget unit. It is the stand-in for the fuel
// meter wazero does not have (runtime.go, Limits): a plugin cannot sit in a
// tight loop hammering the database for the length of its wall-clock budget.
func (inv *invocation) spend() error {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.calls++
	if inv.calls > inv.budget {
		return fmt.Errorf("%w: %d calls", ErrHostCallBudget, inv.calls)
	}
	return nil
}

// fail records the first host-side refusal so Call can report *why* rather
// than the trap that follows it.
func (inv *invocation) fail(err error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.err == nil {
		inv.err = err
	}
}

// ctx returns the invocation's context with the plugin bound as the actor.
// Every host function that touches data goes through here, so a plugin's
// writes are authorized as the plugin and attributed to it (INV-T2/INV-T4) —
// never as the user who triggered it (decisions §3).
func (inv *invocation) ctx(ctx context.Context) context.Context {
	return authz.WithActor(ctx, inv.plugin.Actor(inv.tenant))
}

// hostFunctions builds the table for one plugin's approved capabilities.
//
// **This is INV-X1's enforcement.** A capability the manifest did not request
// produces no entry here, so a module importing that function fails to
// instantiate — it is not refused at call time, it has nothing to call. The
// per-call checks below are the second gate, not the first.
func hostFunctions(p *Installed) []extism.HostFunction {
	// log is unconditional: writing a line to the server's log is not
	// authority over anything, and a plugin nobody can debug is a plugin
	// nobody can trust.
	fns := []extism.HostFunction{
		jsonHostFn("lasterp_log", hostLog),
	}
	if manifestAllows(p.Manifest, "read") {
		fns = append(fns,
			jsonHostFn("lasterp_object_get", hostObjectGet),
			jsonHostFn("lasterp_object_query", hostObjectQuery),
		)
	}
	if manifestAllows(p.Manifest, "write") {
		fns = append(fns,
			jsonHostFn("lasterp_object_create", hostObjectCreate),
			jsonHostFn("lasterp_object_update", hostObjectUpdate),
		)
	}
	if len(p.Manifest.Capabilities.Secrets) > 0 {
		fns = append(fns, jsonHostFn("lasterp_secret_get", hostSecretGet))
	}
	return fns
}

// manifestAllows reports whether any object capability of this access class
// was requested.
func manifestAllows(m *Manifest, access string) bool {
	for _, o := range m.Capabilities.Objects {
		if o.Access == access {
			return true
		}
	}
	return false
}

// manifestAllowsObject is the declared ceiling for one call: the manifest an
// administrator approved bounds what the plugin may touch, even if its role
// somehow carries more. Authority comes from an approved declaration, never
// from a grant that appeared later (INV-T3).
func manifestAllowsObject(m *Manifest, object, access string) bool {
	for _, o := range m.Capabilities.Objects {
		if o.Type == object && o.Access == access {
			return true
		}
	}
	return false
}

// jsonHostFn wraps a JSON-in/JSON-out handler as an Extism host function. One
// shape for every call keeps the ABI small enough to be obviously right, and
// the PDKs of every language can already do JSON.
func jsonHostFn(name string, handler func(context.Context, *invocation, map[string]any) (any, error)) extism.HostFunction {
	return extism.NewHostFunctionWithStack(name,
		func(ctx context.Context, cp *extism.CurrentPlugin, stack []uint64) {
			out, err := func() (any, error) {
				inv, ok := invocationFromContext(ctx)
				if !ok {
					return nil, errors.New("plugins: host call outside an invocation")
				}
				if err := inv.spend(); err != nil {
					inv.fail(err)
					return nil, err
				}
				raw, err := cp.ReadBytes(stack[0])
				if err != nil {
					return nil, fmt.Errorf("plugins: read host call argument: %w", err)
				}
				var req map[string]any
				if err := json.Unmarshal(raw, &req); err != nil {
					return nil, fmt.Errorf("plugins: host call argument is not JSON: %w", err)
				}
				res, err := handler(ctx, inv, req)
				if err != nil {
					inv.fail(err)
					return nil, err
				}
				return res, nil
			}()

			// The plugin sees a structured error, never a Go error string with
			// server internals in it: an untrusted caller learns that its call
			// was refused, not how the host is put together.
			body := map[string]any{"ok": err == nil}
			if err != nil {
				body["error"] = classify(err)
			} else {
				body["result"] = out
			}
			encoded, mErr := json.Marshal(body)
			if mErr != nil {
				encoded = []byte(`{"ok":false,"error":"internal"}`)
			}
			offset, wErr := cp.WriteBytes(encoded)
			if wErr != nil {
				stack[0] = 0
				return
			}
			stack[0] = offset
		},
		[]extism.ValueType{extism.ValueTypeI64},
		[]extism.ValueType{extism.ValueTypeI64},
	)
}

// classify maps a host-side failure to one of a fixed set of words. The set is
// deliberately coarse: "denied" covers both "you have no capability for this"
// and "that object does not exist for you", so a plugin cannot use refusals to
// enumerate what a tenant holds.
func classify(err error) string {
	switch {
	case errors.Is(err, ErrHostCallBudget):
		return "budget"
	case errors.Is(err, authz.ErrPermissionDenied), errors.Is(err, authz.ErrNoActor),
		errors.Is(err, secrets.ErrForbidden), errors.Is(err, secrets.ErrNotFound),
		errors.Is(err, metadata.ErrRecordNotFound):
		return "denied"
	case errors.Is(err, metadata.ErrValidation):
		return "invalid"
	default:
		return "error"
	}
}

// --- the host functions themselves ---

func hostLog(_ context.Context, inv *invocation, req map[string]any) (any, error) {
	msg, _ := req["message"].(string)
	if len(msg) > 4096 {
		msg = msg[:4096]
	}
	// %q is the sanitizer: plugin output is untrusted input, and an unescaped
	// newline in it forges a log line in a product that sells tamper-evident
	// trails.
	// #nosec G706 -- %q escapes control characters; gosec cannot tell which
	// verb formatted the tainted value.
	log.Printf("plugin %s: %q", inv.plugin.ID, msg)
	return map[string]any{"logged": true}, nil
}

func hostObjectGet(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	object, _ := req["object"].(string)
	id, _ := req["id"].(string)
	crud, err := inv.crudFor(object, "read")
	if err != nil {
		return nil, err
	}
	rec, err := crud.Get(inv.ctx(ctx), inv.host.DB, inv.tenant, id)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func hostObjectQuery(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	object, _ := req["object"].(string)
	limit := 100
	if n, ok := req["limit"].(float64); ok && n > 0 && n < 1000 {
		limit = int(n)
	}
	crud, err := inv.crudFor(object, "read")
	if err != nil {
		return nil, err
	}
	after, _ := req["after"].(string)
	recs, err := crud.ListPage(inv.ctx(ctx), inv.host.DB, inv.tenant, after, limit)
	if err != nil {
		return nil, err
	}
	return recs, nil
}

func hostObjectCreate(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	object, _ := req["object"].(string)
	crud, err := inv.crudFor(object, "write")
	if err != nil {
		return nil, err
	}
	fields, _ := req["record"].(map[string]any)
	rec, err := crud.Create(inv.ctx(ctx), inv.host.DB, inv.tenant, metadata.Record(fields))
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func hostObjectUpdate(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	object, _ := req["object"].(string)
	id, _ := req["id"].(string)
	crud, err := inv.crudFor(object, "write")
	if err != nil {
		return nil, err
	}
	changes, _ := req["changes"].(map[string]any)
	rec, err := crud.Update(inv.ctx(ctx), inv.host.DB, inv.tenant, id, metadata.Record(changes))
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// hostSecretGet is the seam WP-3.0 left open: secrets.Get takes a Grants
// predicate precisely so the plugin host could supply the manifest check here
// instead of AllowAll (WP-3.0-decisions.md §5).
func hostSecretGet(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	name, _ := req["name"].(string)
	reader := secrets.Reader{Kind: "plugin", ID: inv.plugin.ID}
	value, err := secrets.Get(ctx, inv.host.DB, inv.host.Keys, inv.tenant, name, reader, inv.secretGrants)
	if err != nil {
		return nil, err
	}
	// The one place a secret deliberately crosses into plugin memory, which is
	// what `secrets: [name]` in a manifest means and why an administrator
	// approves that list by name.
	return map[string]any{"value": string(value)}, nil
}

// secretGrants is the manifest's secrets list as a secrets.Grants predicate.
func (inv *invocation) secretGrants(reader secrets.Reader, name string) bool {
	if reader.Kind != "plugin" || reader.ID != inv.plugin.ID {
		return false
	}
	for _, allowed := range inv.plugin.Manifest.Capabilities.Secrets {
		if allowed == name {
			return true
		}
	}
	return false
}

// crudFor resolves an object name to its CRUD engine, after the manifest
// ceiling. The authz check that follows lives inside CRUD itself
// (authz.Authorize on every method), so both gates run on every call.
func (inv *invocation) crudFor(object, access string) (*metadata.CRUD, error) {
	if object == "" {
		return nil, errors.New("plugins: object is required")
	}
	if !manifestAllowsObject(inv.plugin.Manifest, object, access) {
		return nil, fmt.Errorf("%w: %s does not declare %s access to %s",
			authz.ErrPermissionDenied, inv.plugin.ID, access, object)
	}
	crud, ok := inv.host.Objects[object]
	if !ok {
		// Same word as a refusal: whether an object exists in this deployment
		// is not something an untrusted module needs to learn.
		return nil, fmt.Errorf("%w: unknown object", authz.ErrPermissionDenied)
	}
	return crud, nil
}
