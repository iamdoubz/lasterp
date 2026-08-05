// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Dispatcher implements metadata.Hooks: it runs a tenant's sync plugin hooks
// in the write path, before any transaction exists (WP-3.1b-decisions.md §1).
//
// It is the whole of INV-X2's enforcement, and that enforcement is structural:
// there is no transaction here to partially commit, because CRUD calls this
// before opening one.
type Dispatcher struct {
	host  Host
	stats *Stats

	mu     sync.Mutex
	cached map[tenancy.ID]cachedPlugins
}

type cachedPlugins struct {
	plugins []Installed
	at      time.Time
}

// cacheTTL bounds how stale a dispatcher's view of installed plugins can be.
// Install and Uninstall clear this process's cache immediately; the TTL is what
// covers *another* node's install, which this process never hears about. Five
// seconds of a newly-installed hook not firing is a fair price for not querying
// the plugin table on every write in the product.
const cacheTTL = 5 * time.Second

// NewDispatcher builds the sync hook dispatcher.
func NewDispatcher(h Host) *Dispatcher {
	d := &Dispatcher{stats: NewStats(), cached: map[tenancy.ID]cachedPlugins{}}
	// A plugin's own writes dispatch other plugins' hooks: the host must not be
	// a way around the seam. Self-suppression is what keeps this from
	// recursing (Before).
	h.Hooks = d
	d.host = h
	return d
}

// Stats exposes per-plugin latency and failure counters for the management API.
func (d *Dispatcher) Stats() *Stats { return d.stats }

// Before runs every sync hook registered for (object, verb).
//
// Hooks run in manifest order, each seeing the previous one's output, so two
// enrichment hooks compose. A rejection stops the chain immediately — there is
// no point asking the next plugin about a write that is not happening.
func (d *Dispatcher) Before(ctx context.Context, tenant tenancy.ID, object, verb string, rec metadata.Record) (metadata.Record, error) {
	installed, err := d.pluginsFor(ctx, tenant)
	if err != nil {
		// A dispatcher that cannot see its plugins must not silently let writes
		// through as if none were installed — that is a fail-open on the whole
		// hook surface.
		return nil, fmt.Errorf("plugins: load hooks: %w", err)
	}
	if len(installed) == 0 {
		return rec, nil
	}
	self := currentPluginPrincipal(ctx)

	for i := range installed {
		p := &installed[i]
		hooks := p.Manifest.SyncHooks(object, verb)
		if len(hooks) == 0 {
			continue
		}
		// A plugin does not react to its own writes (decisions §6). Cutting the
		// loop at the source rather than bounding its depth: a plugin whose
		// hook writes the object it hooks would otherwise recurse until
		// something else stopped it.
		if self != "" && self == p.Principal() {
			continue
		}
		for _, h := range hooks {
			out, err := d.runHook(ctx, tenant, p, h, object, verb, rec)
			if err != nil {
				return nil, err
			}
			if out != nil {
				rec = out
			}
		}
	}
	return rec, nil
}

// hookRequest is what a sync hook receives.
type hookRequest struct {
	Object string          `json:"object"`
	Verb   string          `json:"verb"`
	Record metadata.Record `json:"record"`
}

// hookReply is what it may answer with. Both fields are optional: an empty
// reply means "no opinion, proceed unchanged".
type hookReply struct {
	// Reject, when non-empty, refuses the write and is shown to the user.
	Reject string `json:"reject"`
	// Record, when non-nil, replaces the record being written. It is
	// re-validated by CRUD before it is stored (INV-T5).
	Record metadata.Record `json:"record"`
}

// ErrHookRejected is the sentinel a vetoed write carries. internal/app maps it
// to 422 — a well-formed request refused by a business rule, which is what a
// veto is.
var ErrHookRejected = errors.New("plugins: rejected by a plugin hook")

func (d *Dispatcher) runHook(ctx context.Context, tenant tenancy.ID, p *Installed, h Hook, object, verb string, rec metadata.Record) (metadata.Record, error) {
	// The breaker only ever skips hooks that declared themselves skippable.
	// Skipping a fail-closed hook because it is failing would turn a rule that
	// must hold into one that stops holding exactly when its plugin is
	// misbehaving (decisions §3).
	if !h.FailsClosed() && p.BreakerOpen() {
		d.stats.Record(p.ID, h.Fn, 0, outcomeSkipped)
		return nil, nil
	}

	body, err := json.Marshal(hookRequest{Object: object, Verb: verb, Record: rec})
	if err != nil {
		return nil, err
	}

	host := d.host
	host.Limits.Timeout = h.Timeout()

	start := time.Now()
	out, callErr := Call(ctx, host, tenant, p, h.Fn, body)
	elapsed := time.Since(start)

	if callErr != nil {
		d.stats.Record(p.ID, h.Fn, elapsed, outcomeFailed)
		if err := d.recordFailure(ctx, tenant, p); err != nil {
			return nil, err
		}
		if h.FailsClosed() {
			// The plugin is named on purpose: a tenant whose invoicing is down
			// needs to know which plugin is doing it.
			return nil, fmt.Errorf("%w: %s.%s failed and is declared on_failure: reject: %w",
				ErrHookRejected, p.ID, h.Fn, callErr)
		}
		return nil, nil // declared skippable: the write proceeds
	}

	var reply hookReply
	if len(out) > 0 {
		if err := json.Unmarshal(out, &reply); err != nil {
			// A malformed reply is a broken hook, handled exactly like one.
			d.stats.Record(p.ID, h.Fn, elapsed, outcomeFailed)
			if err := d.recordFailure(ctx, tenant, p); err != nil {
				return nil, err
			}
			if h.FailsClosed() {
				return nil, fmt.Errorf("%w: %s.%s returned a reply this host cannot read",
					ErrHookRejected, p.ID, h.Fn)
			}
			return nil, nil
		}
	}

	d.stats.Record(p.ID, h.Fn, elapsed, outcomeOK)
	if err := d.recordSuccess(ctx, tenant, p); err != nil {
		return nil, err
	}

	if reply.Reject != "" {
		// A deliberate veto is not a failure: the plugin worked exactly as
		// installed, so it does not count toward the breaker.
		return nil, fmt.Errorf("%w: %s: %s", ErrHookRejected, p.ID, reply.Reject)
	}
	return reply.Record, nil
}

// pluginsFor returns the tenant's installed plugins, cached briefly.
func (d *Dispatcher) pluginsFor(ctx context.Context, tenant tenancy.ID) ([]Installed, error) {
	d.mu.Lock()
	if c, ok := d.cached[tenant]; ok && time.Since(c.at) < cacheTTL {
		d.mu.Unlock()
		return c.plugins, nil
	}
	d.mu.Unlock()

	loaded, err := LoadAll(ctx, d.host.DB, tenant)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.cached[tenant] = cachedPlugins{plugins: loaded, at: time.Now()}
	d.mu.Unlock()
	return loaded, nil
}

// Forget drops a tenant's cached plugins, so an install or uninstall on this
// node takes effect immediately rather than at the TTL.
func (d *Dispatcher) Forget(tenant tenancy.ID) {
	d.mu.Lock()
	delete(d.cached, tenant)
	d.mu.Unlock()
}

// currentPluginPrincipal returns the acting plugin principal, or "" when the
// actor is a human, an agent, or absent.
func currentPluginPrincipal(ctx context.Context) string {
	actor, err := authz.ActorFromContext(ctx)
	if err != nil {
		return ""
	}
	id := string(actor.UserID)
	if len(id) > len(principalPrefix) && id[:len(principalPrefix)] == principalPrefix {
		return id
	}
	return ""
}

// recordFailure advances the breaker, opening it at the threshold.
func (d *Dispatcher) recordFailure(ctx context.Context, tenant tenancy.ID, p *Installed) error {
	return bumpBreaker(ctx, d.host.DB, tenant, p, +1)
}

// recordSuccess closes a breaker that a working call has vindicated.
func (d *Dispatcher) recordSuccess(ctx context.Context, tenant tenancy.ID, p *Installed) error {
	if p.HookFailures == 0 && p.BreakerOpenedAt == nil {
		return nil // the common path writes nothing
	}
	return bumpBreaker(ctx, d.host.DB, tenant, p, 0)
}

var _ metadata.Hooks = (*Dispatcher)(nil)

// Host returns the dispatcher's host, for callers that invoke plugins directly
// (the management API's call route) and must use the same wiring hooks do.
func (d *Dispatcher) Host() Host { return d.host }
