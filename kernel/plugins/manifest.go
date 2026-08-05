// SPDX-License-Identifier: AGPL-3.0-only

// Package plugins is the WP-3.1a plugin host (ADR-007, docs/05): untrusted
// WebAssembly running on wazero through Extism, with no authority except what
// a manifest declared and an administrator approved.
//
// The shape of the security model, in one place:
//
//   - A plugin is its **own principal** (`plugin:<id>`), never the user who
//     triggered it, so an audit row names the plugin and its power does not
//     vary by caller (WP-3.1-decisions.md §3).
//   - Its permissions are **intersection(manifest, approver's own grants)** —
//     an approval may narrow, never widen (INV-T3).
//   - The host-function table is **built from** the approved capabilities, so a
//     plugin that asks for a function it was not granted fails to *instantiate*
//     rather than being refused at call time (INV-X1).
//   - Every host call re-checks authz for the specific object anyway. Two
//     gates: a human reviews the manifest once, authz enforces every call.
package plugins

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is a plugin's declaration (docs/05 §Plugin manifest). It is the
// document an administrator reads before approving anything, so it is parsed
// strictly: a field this host does not implement is an error, never a silent
// no-op. A plugin author who declares a hook and gets an install that quietly
// ignores it has been lied to.
type Manifest struct {
	ID      string `yaml:"id"`
	Version string `yaml:"version"`
	// LastERP is the host version range the author claims (docs/05). Recorded
	// but not enforced: version solving is WP-3.2's registry work.
	LastERP string `yaml:"lasterp"`
	// Functions are the exported WASM functions this plugin may be called on.
	// An export not listed here is unreachable — the callable surface is
	// declared, not discovered.
	Functions    []string     `yaml:"functions"`
	Capabilities Capabilities `yaml:"capabilities"`
	// Hooks are the write-path subscriptions (WP-3.1b).
	Hooks []Hook `yaml:"hooks"`

	// Declared-but-unimplemented surfaces. Parsed so install can refuse them by
	// name (see Validate); each lands with a named WP.
	Overlays  []string         `yaml:"overlays"`
	MCPTools  []map[string]any `yaml:"mcp_tools"`
	Endpoints []map[string]any `yaml:"endpoints"`
}

// Hook is one write-path subscription.
type Hook struct {
	// Event is `<Object>.before_create|before_update|before_delete` for a sync
	// hook, or `<Object>.changed` for an async one.
	Event string `yaml:"event"`
	// Fn is the exported function to call. It must also appear in Functions —
	// the callable surface is declared once.
	Fn string `yaml:"fn"`
	// Mode is "sync" (in the write path, may veto) or "async" (delivered from
	// the change feed after the commit).
	Mode string `yaml:"mode"`
	// OnFailure decides what a *broken* sync hook means: "reject" (the
	// default) fails the write, "allow" records the failure and lets it
	// through. Both are correct for different hooks — a compliance rule must
	// fail closed, an enrichment must not take invoicing down — so the hook
	// declares which it is rather than the host guessing (decisions §3).
	OnFailure string `yaml:"on_failure"`
	// TimeoutMS overrides the 50ms sync default, up to MaxHookTimeoutMS.
	// Raising it is a cost the installing administrator is shown in plain
	// language, because the person who feels the latency is usually not the
	// person who installed the plugin.
	TimeoutMS int `yaml:"timeout_ms"`
}

// Hook modes and failure policies.
const (
	ModeSync  = "sync"
	ModeAsync = "async"

	OnFailureReject = "reject"
	OnFailureAllow  = "allow"

	// DefaultHookTimeoutMS is the sync budget a hook gets unless it asks for
	// more. docs/09 promises p95 < 300ms for writes; 50ms leaves that promise
	// intact for a plugin that does not ask otherwise.
	DefaultHookTimeoutMS = 50
	// MaxHookTimeoutMS is the ceiling no manifest may exceed — docs/05's
	// original sync-hook default, which is a ceiling rather than a default here.
	MaxHookTimeoutMS = 500
)

// Sync hook verbs, mirroring metadata's.
var syncEvents = map[string]string{
	"before_create": "create",
	"before_update": "update",
	"before_delete": "delete",
}

// AsyncEvent is the only async subscription: the feed says *what* changed, not
// what was done to it (there is no verb on a feed entry), so a hook binds to
// the object and reads current state to decide (decisions §2).
const AsyncEvent = "changed"

// Object returns the object a hook subscribes to, and the verb it fires on.
// For an async hook the verb is AsyncEvent.
func (h Hook) Object() (object, verb string) {
	i := strings.LastIndex(h.Event, ".")
	if i < 0 {
		return "", ""
	}
	return h.Event[:i], h.Event[i+1:]
}

// Timeout is the hook's sync budget.
func (h Hook) Timeout() time.Duration {
	ms := h.TimeoutMS
	if ms <= 0 {
		ms = DefaultHookTimeoutMS
	}
	return time.Duration(ms) * time.Millisecond
}

// FailsClosed reports whether a broken hook rejects the write.
func (h Hook) FailsClosed() bool { return h.OnFailure != OnFailureAllow }

// SyncHooks returns the manifest's sync hooks for one object and CRUD verb.
func (m *Manifest) SyncHooks(object, verb string) []Hook {
	var out []Hook
	for _, h := range m.Hooks {
		if h.Mode != ModeSync {
			continue
		}
		o, event := h.Object()
		if o == object && syncEvents[event] == verb {
			out = append(out, h)
		}
	}
	return out
}

// AsyncHooks returns the manifest's async hooks for one object.
func (m *Manifest) AsyncHooks(object string) []Hook {
	var out []Hook
	for _, h := range m.Hooks {
		if h.Mode != ModeAsync {
			continue
		}
		if o, event := h.Object(); o == object && event == AsyncEvent {
			out = append(out, h)
		}
	}
	return out
}

// HasAsyncHooks reports whether this plugin needs the delivery runner at all.
func (m *Manifest) HasAsyncHooks() bool {
	for _, h := range m.Hooks {
		if h.Mode == ModeAsync {
			return true
		}
	}
	return false
}

// Capabilities is the requested authority. Everything absent here is absent
// from the sandbox.
type Capabilities struct {
	Objects []ObjectAccess `yaml:"objects"`
	Secrets []string       `yaml:"secrets"`
	// HTTP is refused at install in WP-3.1a — see Validate.
	HTTP []map[string]any `yaml:"http"`
	// Schedule is refused likewise (no job runner until WP-3.1b).
	Schedule []string `yaml:"schedule"`
}

// ObjectAccess is one object-type grant request.
type ObjectAccess struct {
	Type   string `yaml:"type"`
	Access string `yaml:"access"` // "read" or "write"
}

// idRE bounds plugin ids: they become a principal name, a role name, an audit
// actor and a URL segment.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,127}$`)

// fnRE bounds exported function names.
var fnRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// ParseManifest parses and validates a manifest.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // an unknown key is a typo or a capability we do not implement
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plugins: parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate rejects manifests this host cannot honour exactly.
func (m *Manifest) Validate() error {
	if !idRE.MatchString(m.ID) {
		return fmt.Errorf("plugins: %q is not a valid plugin id", m.ID)
	}
	if m.Version == "" {
		return errors.New("plugins: manifest needs a version")
	}
	if len(m.Functions) == 0 {
		return errors.New("plugins: manifest declares no functions, so nothing could ever call it")
	}
	for _, fn := range m.Functions {
		if !fnRE.MatchString(fn) {
			return fmt.Errorf("plugins: %q is not a valid function name", fn)
		}
	}
	for _, o := range m.Capabilities.Objects {
		if o.Type == "" {
			return errors.New("plugins: an object capability needs a type")
		}
		if o.Access != "read" && o.Access != "write" {
			return fmt.Errorf("plugins: object %s: access must be read or write, got %q", o.Type, o.Access)
		}
	}
	for _, s := range m.Capabilities.Secrets {
		if s == "" {
			return errors.New("plugins: a secret capability needs a name")
		}
	}

	for i, h := range m.Hooks {
		if err := h.validate(m); err != nil {
			return fmt.Errorf("plugins: hook %d (%s): %w", i, h.Event, err)
		}
	}

	// Refused rather than ignored. Silently dropping a declared capability is
	// the failure mode where an author ships a plugin that "installs fine" and
	// never fires, and the same mechanism could one day drop a capability an
	// administrator thought they were reviewing.
	for _, unsupported := range []struct {
		present bool
		what    string
		owner   string
	}{
		{len(m.Endpoints) > 0, "endpoints", "WP-3.2, alongside the example plugins that call them"},
		{len(m.Capabilities.Schedule) > 0, "capabilities.schedule", "WP-3.3, which owns the job runner"},
		{len(m.Capabilities.HTTP) > 0, "capabilities.http", "the WP that adds an audited outbound client (ADR-007 requires every call be audited; the sandbox has no network at all until then)"},
		{len(m.Overlays) > 0, "overlays", "WP-3.2 (bundle install)"},
		{len(m.MCPTools) > 0, "mcp_tools", "WP-3.4 (MCP server)"},
	} {
		if unsupported.present {
			return fmt.Errorf("plugins: manifest declares %s, which this host does not implement yet — it lands with %s. Refusing rather than installing a plugin whose declaration is partly ignored", unsupported.what, unsupported.owner)
		}
	}
	return nil
}

// Permissions is the manifest's object capabilities as authz (object, action)
// tuples — the vocabulary the rest of the kernel already speaks.
//
// `read` maps to read and `write` to create+update, literally. A writer that
// also needs to read declares both, because least privilege is the point and
// inferring the extra grant would widen what the administrator approved.
func (m *Manifest) Permissions() [][2]string {
	seen := map[[2]string]bool{}
	var out [][2]string
	add := func(object, action string) {
		key := [2]string{object, action}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	for _, o := range m.Capabilities.Objects {
		switch o.Access {
		case "read":
			add(o.Type, "read")
		case "write":
			add(o.Type, "create")
			add(o.Type, "update")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

// Principal is the plugin's own actor id (decisions §3). It is deliberately
// not a users row: nothing can log in as a plugin, because no login path can
// find a principal that does not exist in `users`.
func (m *Manifest) Principal() string { return PrincipalFor(m.ID) }

// PrincipalFor is Principal for a plugin id already parsed.
func PrincipalFor(id string) string { return "plugin:" + id }

// validate checks one hook declaration against the host's vocabulary.
func (h Hook) validate(m *Manifest) error {
	object, event := h.Object()
	if object == "" || event == "" {
		return errors.New("event must be `<Object>.<event>`")
	}
	if !declaredFn(m.Functions, h.Fn) {
		return fmt.Errorf("fn %q is not in this manifest's functions list", h.Fn)
	}
	switch h.Mode {
	case ModeSync:
		if _, ok := syncEvents[event]; !ok {
			return fmt.Errorf("%q is not a sync event (want before_create, before_update or before_delete)", event)
		}
	case ModeAsync:
		if event != AsyncEvent {
			// The feed carries no verb, so `Invoice.posted` is not something
			// this host can honour — and pretending to would deliver on every
			// change instead. Refused by name rather than approximated
			// (WP-3.1b-decisions.md §2).
			return fmt.Errorf("%q is not an async event: the change feed records that an object changed, not what was done to it, so async hooks bind to `%s.%s` and read current state", event, object, AsyncEvent)
		}
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeSync, ModeAsync, h.Mode)
	}
	switch h.OnFailure {
	case "", OnFailureReject, OnFailureAllow:
	default:
		return fmt.Errorf("on_failure must be %q or %q, got %q", OnFailureReject, OnFailureAllow, h.OnFailure)
	}
	if h.TimeoutMS < 0 || h.TimeoutMS > MaxHookTimeoutMS {
		return fmt.Errorf("timeout_ms must be between 1 and %d (a sync hook runs inside every write of %s; %dms is the ceiling because past it the write budget in docs/09 is gone)",
			MaxHookTimeoutMS, object, MaxHookTimeoutMS)
	}
	return nil
}

func declaredFn(list []string, fn string) bool {
	for _, s := range list {
		if s == fn {
			return true
		}
	}
	return false
}

// LatencyWarning is the plain-language cost of a hook, for the approval screen.
// It is returned for every sync hook at install so the administrator sees what
// they are agreeing to *before* it is running in production — the person who
// feels the latency is usually not the person who installed the plugin.
func (h Hook) LatencyWarning() string {
	if h.Mode != ModeSync {
		return ""
	}
	object, _ := h.Object()
	return fmt.Sprintf(
		"every write of %s will wait for %s (up to %v) before it is saved; if this plugin is slow or unreachable, saving a %s is slow for everyone in this tenant",
		object, h.Fn, h.Timeout(), object)
}
