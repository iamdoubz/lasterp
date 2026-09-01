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

	"github.com/iamdoubz/lasterp/kernel/jobs"
)

// Manifest is a plugin's declaration (docs/05 §Plugin manifest). It is the
// document an administrator reads before approving anything, so it is parsed
// strictly: a field this host does not implement is an error, never a silent
// no-op. A plugin author who declares a hook and gets an install that quietly
// ignores it has been lied to.
type Manifest struct {
	ID      string `yaml:"id"`
	Version string `yaml:"version"`
	// LastERP is the host version range the author claims (docs/05), enforced
	// at install from WP-3.2b — see version.go. An empty range makes no claim
	// and is satisfied by any host.
	LastERP string `yaml:"lasterp"`
	// Functions are the exported WASM functions this plugin may be called on.
	// An export not listed here is unreachable — the callable surface is
	// declared, not discovered.
	Functions    []string     `yaml:"functions"`
	Capabilities Capabilities `yaml:"capabilities"`
	// Hooks are the write-path subscriptions (WP-3.1b).
	Hooks []Hook `yaml:"hooks"`

	// Endpoints are the HTTP routes this plugin serves under
	// `/ext/<id>/` (WP-3.2a).
	Endpoints []Endpoint `yaml:"endpoints"`

	// Overlays names the shipped objects this plugin customizes (WP-3.2c). One
	// entry per object; the bundle carries the document for each as a flat
	// `overlay.<Object>.yaml` entry, and the document names the same object
	// again. Three places agree or the bundle is refused — an overlay that can
	// be retargeted by renaming a file is one an administrator did not approve.
	//
	// Object names, not file names, because this is the line the approving
	// administrator reads: "customizes Contact" is a decision they can make,
	// "carries overlays/a.yaml" is not.
	Overlays []string `yaml:"overlays"`

	// Declared-but-unimplemented surfaces. Parsed so install can refuse them by
	// name (see Validate); each lands with a named WP.
	MCPTools []map[string]any `yaml:"mcp_tools"`
}

// Endpoint is one route the plugin serves under `/ext/<id>/`.
//
// The surface is declared, never discovered — the same rule as Functions. An
// administrator approving an install can read every path this plugin will
// answer on, and `GET /api/v1/plugins` lists them afterwards, because "what
// does this thing expose" has to be answerable without reading the module.
type Endpoint struct {
	// Path is the route under the plugin's prefix, e.g. "/report" serves
	// `/ext/com.acme.x/report`. Matched exactly.
	Path string `yaml:"path"`
	// Fn is the exported function to call. It must also appear in Functions.
	Fn string `yaml:"fn"`
	// Methods defaults to GET. Only GET and POST are routable: those are the
	// two patterns the gateway registers, and a declared PUT that silently
	// never fires is the failure mode Validate exists to prevent.
	Methods []string `yaml:"methods"`
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
	// HTTP is the outbound allowlist (WP-3.2a). Empty means the sandbox has
	// no network at all, which is every plugin's default.
	HTTP []HTTPHost `yaml:"http"`
	// Schedule is a list of 5-field cron expressions (WP-3.3b). Each becomes a
	// job_schedules row owned by this plugin's principal, firing its first
	// declared function. Parsed at install, so an expression that cannot fire
	// is refused where the administrator can see it.
	Schedule []string `yaml:"schedule"`
	// Jobs unlocks `enqueue_job`: this plugin may queue its own functions to
	// run outside a request.
	//
	// Its own field rather than a side effect of Schedule, because the two say
	// different things. Schedule is "run me at 2am"; Jobs is "let me defer my
	// own work", which is what a hook that must not block a write needs and
	// which has nothing to do with cron. Folding them together would make an
	// author write a fake cron expression to get deferral — and would make
	// `schedule:` mean two things to whoever reviews it.
	//
	// Declaring Schedule implies Jobs: a plugin that runs on a timer already
	// runs outside a request, so refusing it the queue would be a distinction
	// without a difference (see AllowsJobs).
	Jobs bool `yaml:"jobs"`
}

// AllowsJobs reports whether this plugin may enqueue background work.
func (c Capabilities) AllowsJobs() bool { return c.Jobs || len(c.Schedule) > 0 }

// HTTPHost is one outbound destination the plugin asks for.
//
// A host, not a URL prefix: the allowlist an administrator reviews should read
// as "this plugin talks to api.acme.com", and a path-scoped allowlist implies
// a containment it cannot deliver (the same host serves whatever paths it
// likes). The port defaults to 443 and may be given as `host:port`; the scheme
// is always https (http.go).
type HTTPHost struct {
	Host    string   `yaml:"host"`
	Methods []string `yaml:"methods"`
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
	if err := checkHostVersion(m.LastERP); err != nil {
		return err
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

	for i := range m.Capabilities.HTTP {
		if err := m.Capabilities.HTTP[i].validate(); err != nil {
			return fmt.Errorf("plugins: capabilities.http[%d]: %w", i, err)
		}
	}

	// Cron is parsed here rather than by the runner. A schedule the runner
	// cannot read is a plugin that installs cleanly and never fires — the exact
	// failure this whole Validate exists to prevent — and "can never fire" (the
	// 30th of February) is as much a mistake as a syntax error.
	for i, expr := range m.Capabilities.Schedule {
		if err := jobs.ValidCron(expr); err != nil {
			return fmt.Errorf("plugins: capabilities.schedule[%d]: %w", i, err)
		}
	}
	if len(m.Capabilities.Schedule) > 0 && len(m.Functions) == 0 {
		return errors.New("plugins: capabilities.schedule needs a function to call")
	}

	seenPath := map[string]bool{}
	for i := range m.Endpoints {
		e := &m.Endpoints[i]
		if err := e.validate(m); err != nil {
			return fmt.Errorf("plugins: endpoint %d (%s): %w", i, e.Path, err)
		}
		if seenPath[e.Path] {
			return fmt.Errorf("plugins: endpoint %s is declared twice; one path has one handler", e.Path)
		}
		seenPath[e.Path] = true
	}

	// Refused rather than ignored. Silently dropping a declared capability is
	// the failure mode where an author ships a plugin that "installs fine" and
	// never fires, and the same mechanism could one day drop a capability an
	// administrator thought they were reviewing.
	seenOverlay := map[string]bool{}
	for _, object := range m.Overlays {
		if object == "" {
			return errors.New("plugins: overlays entries are object names; one is empty")
		}
		if seenOverlay[object] {
			return fmt.Errorf("plugins: overlays declares %s twice; one object has one overlay per plugin", object)
		}
		seenOverlay[object] = true
	}

	for _, unsupported := range []struct {
		present bool
		what    string
		owner   string
	}{
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

// Methods with a default: an endpoint that names none serves GET.
func (e Endpoint) methods() []string {
	if len(e.Methods) == 0 {
		return []string{"GET"}
	}
	return e.Methods
}

// Serves reports whether this endpoint answers that method.
func (e Endpoint) Serves(method string) bool {
	for _, m := range e.methods() {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// endpointPathRE bounds a declared route. No query, no fragment, no dot
// segments, no encoded bytes: the path an administrator reads in the manifest
// is the path the server routes, with nothing in between to decode
// differently.
var endpointPathRE = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9/_.\-]{0,127}$`)

func (e *Endpoint) validate(m *Manifest) error {
	if !endpointPathRE.MatchString(e.Path) || strings.Contains(e.Path, "..") || strings.Contains(e.Path, "//") {
		return fmt.Errorf("path %q must be an absolute route under the plugin prefix, e.g. /report", e.Path)
	}
	if !declaredFn(m.Functions, e.Fn) {
		return fmt.Errorf("fn %q is not in this manifest's functions list", e.Fn)
	}
	for _, method := range e.methods() {
		switch strings.ToUpper(method) {
		case "GET", "POST":
		default:
			// Refused rather than dropped: the gateway registers GET and POST
			// under /ext, so a declared PUT would install cleanly and never be
			// reachable — the silent-no-op failure ParseManifest exists to
			// prevent.
			return fmt.Errorf("method %q is not routable under /ext (GET and POST are)", method)
		}
	}
	return nil
}

// Endpoint returns the declared endpoint for a path, if any.
func (m *Manifest) Endpoint(path string) (Endpoint, bool) {
	for _, e := range m.Endpoints {
		if e.Path == path {
			return e, true
		}
	}
	return Endpoint{}, false
}

// hostPortRE bounds an outbound allowlist entry: a DNS name, optionally with a
// port. No wildcards — `*.acme.com` reads like containment and is not (whoever
// controls the zone controls the destination), so an author lists the hosts
// they actually call.
var hostPortRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.\-]{0,253}[a-z0-9])?(:[0-9]{1,5})?$`)

func (h *HTTPHost) validate() error {
	h.Host = strings.ToLower(strings.TrimSpace(h.Host))
	if !hostPortRE.MatchString(h.Host) {
		return fmt.Errorf("host %q must be a DNS name, optionally with a port (no scheme, no path, no wildcard)", h.Host)
	}
	for _, method := range h.methods() {
		switch strings.ToUpper(method) {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		default:
			return fmt.Errorf("method %q is not an HTTP method this host will make", method)
		}
	}
	return nil
}

func (h HTTPHost) methods() []string {
	if len(h.Methods) == 0 {
		return []string{"GET"}
	}
	return h.Methods
}

// AllowsHTTP reports whether the manifest declared this exact destination and
// method. hostPort is the request's host with its port made explicit, so
// `api.acme.com` in a manifest means port 443 and nothing else.
func (m *Manifest) AllowsHTTP(method, hostPort string) bool {
	hostPort = strings.ToLower(hostPort)
	for _, h := range m.Capabilities.HTTP {
		want := h.Host
		if !strings.Contains(want, ":") {
			want += ":443"
		}
		if want != hostPort {
			continue
		}
		for _, allowed := range h.methods() {
			if strings.EqualFold(allowed, method) {
				return true
			}
		}
	}
	return false
}
