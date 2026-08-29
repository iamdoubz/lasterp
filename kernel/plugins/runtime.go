// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Limits are the sandbox's resource bounds (docs/05 §Host runtime rules).
//
// **There is no fuel meter, and this is the honest list.** docs/05 promised
// "CPU fuel metering"; wazero has no fuel counter — that is a wasmtime feature
// — and its interruption mechanism is context cancellation, which is
// wall-clock. So a runaway plugin is stopped by Timeout, an allocating one by
// MaxPages, and one spinning through the host API by MaxHostCalls, which is
// the cheap stand-in for what fuel would otherwise catch. docs/05 is amended
// in this WP rather than left promising a knob the runtime does not have
// (WP-3.1-decisions.md §2).
type Limits struct {
	// MaxPages caps linear memory in 64KiB WASM pages. 1024 = 64MiB, the
	// docs/05 default.
	MaxPages uint32
	// Timeout is the wall-clock budget for one call. docs/05's sync-hook
	// default; WP-3.1b's async hooks and jobs get their own.
	Timeout time.Duration
	// MaxHostCalls bounds how many host functions one invocation may call.
	MaxHostCalls int
}

// DefaultLimits is docs/05's default posture.
var DefaultLimits = Limits{MaxPages: 1024, Timeout: 500 * time.Millisecond, MaxHostCalls: 1000}

func (l Limits) withDefaults() Limits {
	if l.MaxPages == 0 {
		l.MaxPages = DefaultLimits.MaxPages
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultLimits.Timeout
	}
	if l.MaxHostCalls == 0 {
		l.MaxHostCalls = DefaultLimits.MaxHostCalls
	}
	return l
}

// Host is what a plugin invocation is allowed to reach: the database, the
// objects it may address, and the secrets key source. It is passed in by the
// composition root rather than reached for, so kernel/plugins has no ambient
// authority of its own either.
type Host struct {
	DB      *storage.DB
	Objects map[string]*metadata.CRUD
	Keys    secrets.KeySource
	Limits  Limits
	// HTTP is the deployment's outbound posture (http.go). The zero value is
	// the safe one: public destinations only.
	HTTP HTTPPolicy
	// Hooks makes a plugin's own writes dispatch *other* plugins' hooks. Set
	// by NewDispatcher, so a write from inside a host call is subject to the
	// same hook surface as a write from the API — otherwise the plugin host
	// would be the bypass the seam was designed to avoid. Recursion is not a
	// risk: dispatch self-suppresses on the acting plugin (dispatch.go).
	Hooks metadata.Hooks
}

// ErrFunctionNotDeclared is returned when a caller names an export the
// manifest did not declare. The callable surface is declared, not discovered:
// a module may export anything, and only what an administrator approved is
// reachable.
var ErrFunctionNotDeclared = errors.New("plugins: function is not declared in the manifest")

// ErrHostCallBudget is returned when an invocation exceeds MaxHostCalls.
var ErrHostCallBudget = errors.New("plugins: host call budget exhausted")

// Call runs one exported function and returns its output.
//
// Everything the plugin can do is decided before this returns: the host
// function table is built from the approved capabilities (so an ungranted
// function is not merely refused, it does not exist to import), the module is
// instantiated with no filesystem, no network, no environment and no
// arguments, and the whole call runs under a deadline.
func Call(ctx context.Context, h Host, tenant tenancy.ID, p *Installed, fn string, input []byte) ([]byte, error) {
	if h.DB == nil || tenant == "" || p == nil {
		return nil, errors.New("plugins: host, tenant and plugin are required")
	}
	if !declared(p.Manifest.Functions, fn) {
		return nil, fmt.Errorf("%w: %s", ErrFunctionNotDeclared, fn)
	}
	limits := h.Limits.withDefaults()

	compiled, err := compile(ctx, p, limits)
	if err != nil {
		return nil, err
	}

	inv := &invocation{host: h, tenant: tenant, plugin: p, budget: limits.MaxHostCalls}
	ctx, cancel := context.WithTimeout(withInvocation(ctx, inv), limits.Timeout)
	defer cancel()

	// A fresh instance per call: no state survives between invocations, so one
	// tenant's call cannot leave anything behind for the next (docs/05 allows
	// pooling with reset; per-invocation is the version with nothing to get
	// wrong).
	instance, err := compiled.Instance(ctx, extism.PluginInstanceConfig{
		// Deliberately empty: wazero's zero ModuleConfig mounts no filesystem,
		// exports no environment and passes no arguments. The sandbox starts
		// with nothing and the capability table is the only thing added to it.
		ModuleConfig: wazero.NewModuleConfig(),
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: instantiate %s: %w", p.ID, err)
	}
	defer func() { _ = instance.Close(ctx) }()

	_, out, err := instance.CallWithContext(ctx, fn, input)
	if err != nil {
		if inv.err != nil {
			// A host-side refusal (no capability, no permission, budget) is the
			// real reason; the WASM trap that followed is a symptom.
			return nil, inv.err
		}
		return nil, fmt.Errorf("plugins: %s.%s: %w", p.ID, fn, err)
	}
	return out, nil
}

// compiled plugins are cached: compiling a couple of megabytes of WASM takes
// hundreds of milliseconds, which would blow the docs/09 write budget on every
// call. The key includes the capability shape because the host function table
// is baked in at compile time — two installs of the same bytes with different
// approvals are different modules.
var (
	compiledMu sync.Mutex
	compiledBy = map[string]*extism.CompiledPlugin{}
)

func compile(ctx context.Context, p *Installed, limits Limits) (*extism.CompiledPlugin, error) {
	key := p.SHA256 + "|" + capabilitySignature(p) + "|" + fmt.Sprint(limits.MaxPages, limits.Timeout)

	compiledMu.Lock()
	defer compiledMu.Unlock()
	if c, ok := compiledBy[key]; ok {
		return c, nil
	}

	manifest := extism.Manifest{
		Wasm:   []extism.Wasm{extism.WasmData{Data: p.module}},
		Memory: &extism.ManifestMemory{MaxPages: limits.MaxPages},
		// Milliseconds. Set on the manifest as well as on the context so
		// wazero is configured to close on context-done — the mechanism that
		// actually kills an infinite loop. withDefaults has already made the
		// duration positive, so the conversion cannot wrap.
		Timeout: uint64(max(limits.Timeout/time.Millisecond, 1)),
		// Extism's own client stays off, permanently. ADR-007 requires every
		// outbound call to be allowlisted *and audited*, and the built-in
		// client does the first only. WP-3.2a's answer is not to enable it but
		// to replace it: `lasterp_http_request` (http.go) is the one way out,
		// so a module that reaches for `extism:host/env::http_request` still
		// finds nothing there.
		AllowedHosts: []string{},
		AllowedPaths: map[string]string{},
	}
	c, err := extism.NewCompiledPlugin(ctx, manifest, extism.PluginConfig{
		// WASI is on because every mainstream toolchain emits modules that
		// import wasi_snapshot_preview1 to start at all. It grants nothing by
		// itself: with no FS mounted, no env and no args, its file and
		// environment calls have nothing to reach.
		EnableWasi: true,
	}, hostFunctions(p))
	if err != nil {
		return nil, fmt.Errorf("plugins: compile %s: %w", p.ID, err)
	}
	compiledBy[key] = c
	return c, nil
}

// forgetCompiled drops every cached compilation of these bytes, so an
// uninstall/reinstall cycle cannot serve a module from before the change.
func forgetCompiled(sha string) {
	compiledMu.Lock()
	defer compiledMu.Unlock()
	for key, c := range compiledBy {
		if strings.HasPrefix(key, sha+"|") {
			_ = c.Close(context.Background())
			delete(compiledBy, key)
		}
	}
}

// capabilitySignature is a stable string for the approved authority, used as
// part of the compilation cache key.
func capabilitySignature(p *Installed) string {
	parts := make([]string, 0, len(p.Granted)+len(p.Manifest.Capabilities.Secrets))
	for _, g := range p.Granted {
		parts = append(parts, g[0]+":"+g[1])
	}
	for _, s := range p.Manifest.Capabilities.Secrets {
		parts = append(parts, "secret/"+s)
	}
	for _, h := range p.Manifest.Capabilities.HTTP {
		parts = append(parts, "http/"+h.Host)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func declared(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
