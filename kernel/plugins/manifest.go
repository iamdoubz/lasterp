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

	// Declared-but-unimplemented surfaces. Parsed so install can refuse them by
	// name (see Validate); each lands with a named WP.
	Hooks     []map[string]any `yaml:"hooks"`
	Overlays  []string         `yaml:"overlays"`
	MCPTools  []map[string]any `yaml:"mcp_tools"`
	Endpoints []map[string]any `yaml:"endpoints"`
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

	// Refused rather than ignored. Silently dropping a declared capability is
	// the failure mode where an author ships a plugin that "installs fine" and
	// never fires, and the same mechanism could one day drop a capability an
	// administrator thought they were reviewing.
	for _, unsupported := range []struct {
		present bool
		what    string
		owner   string
	}{
		{len(m.Hooks) > 0, "hooks", "WP-3.1b (hook dispatch)"},
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
