// SPDX-License-Identifier: AGPL-3.0-only

package capability

import (
	"fmt"
	"sort"
	"strings"
)

// ErrKernelNotDisableable is returned by DisableImpact for a kernel module.
var ErrKernelNotDisableable = fmt.Errorf("capability: kernel modules cannot be disabled")

// Closure returns the full set of modules that must be enabled for every
// module in seeds to run: the transitive requires-closure plus the always-on
// kernel. Deterministic (sorted). It is the deterministic metadata evaluation
// ADR-018 §3 calls for — no hidden coupling.
func (r *Registry) Closure(seeds []string) ([]string, error) {
	enabled := map[string]bool{}
	for _, k := range r.kernel {
		enabled[k] = true
	}
	var queue []string
	push := func(name string) error {
		if _, ok := r.modules[name]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownModule, name)
		}
		if !enabled[name] {
			enabled[name] = true
			queue = append(queue, name)
		}
		return nil
	}
	for _, s := range seeds {
		if err := push(s); err != nil {
			return nil, err
		}
	}
	queue = append(queue, r.kernel...)
	for len(queue) > 0 {
		m := r.modules[queue[0]]
		queue = queue[1:]
		for _, cap := range m.Requires {
			prov, ok := r.providers[cap]
			if !ok {
				// validateGraph already rules this out for base requires; kept
				// as a guard so a future bad manifest fails loudly.
				return nil, fmt.Errorf("capability: %q requires unprovided %q", m.Module, cap)
			}
			if err := push(prov); err != nil {
				return nil, err
			}
		}
	}
	return sortedKeys(enabled), nil
}

// EnableResult is the preview ADR-018 §3 requires: what enabling a module
// additionally turns on, shown before applying.
type EnableResult struct {
	Target string   // the module being enabled
	Added  []string // modules newly enabled by the closure (excludes already-on)
}

// Preview renders the human line, e.g. "Enabling Invoicing will also enable:
// Contacts, Ledger, Tax engine." Added is empty ⇒ "no additional modules".
func (r *Registry) Preview(res EnableResult) string {
	if len(res.Added) == 0 {
		return fmt.Sprintf("Enabling %s needs no additional modules.", r.title(res.Target))
	}
	titles := make([]string, len(res.Added))
	for i, name := range res.Added {
		titles[i] = r.title(name)
	}
	return fmt.Sprintf("Enabling %s will also enable: %s.", r.title(res.Target), strings.Join(titles, ", "))
}

// EnableClosure computes the closure of enabling module on top of the already
// enabled set, returning what would be newly turned on.
func (r *Registry) EnableClosure(enabled []string, module string) (EnableResult, error) {
	if _, err := r.Module(module); err != nil {
		return EnableResult{}, err
	}
	before := set(enabled)
	full, err := r.Closure(append(append([]string{}, enabled...), module))
	if err != nil {
		return EnableResult{}, err
	}
	var added []string
	for _, name := range full {
		if !before[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	return EnableResult{Target: module, Added: added}, nil
}

// DisableImpact reports the enabled modules that depend on module — the
// reverse-dependency check ADR-018 §3 shows before disabling. If the returned
// slice is non-empty, disabling module would break those modules, so the
// caller must refuse or cascade (with the reason shown). Kernel modules
// return ErrKernelNotDisableable.
func (r *Registry) DisableImpact(enabled []string, module string) ([]string, error) {
	m, err := r.Module(module)
	if err != nil {
		return nil, err
	}
	if m.Kernel {
		return nil, ErrKernelNotDisableable
	}
	remaining := map[string]bool{}
	for _, name := range enabled {
		if name != module {
			remaining[name] = true
		}
	}
	// A remaining module is broken if one of its base requires is no longer
	// satisfiable within the remaining set.
	var broken []string
	for name := range remaining {
		rm := r.modules[name]
		if rm == nil || rm.Kernel {
			continue
		}
		for _, cap := range rm.Requires {
			if !r.capabilityCovered(cap, remaining) {
				broken = append(broken, name)
				break
			}
		}
	}
	sort.Strings(broken)
	return broken, nil
}

// AvailableModes returns the names of module's modes whose extra requires are
// all satisfiable within the enabled set; unavailable modes (needing a
// capability no enabled module provides — e.g. documents.ocr before M3) are
// omitted. WP-0.9 decisions §7.
func (r *Registry) AvailableModes(enabled []string, module string) ([]string, error) {
	m, err := r.Module(module)
	if err != nil {
		return nil, err
	}
	on := set(enabled)
	var avail []string
	for _, mode := range m.Modes {
		ok := true
		for _, cap := range mode.Requires {
			if !r.capabilityCovered(cap, on) {
				ok = false
				break
			}
		}
		if ok {
			avail = append(avail, mode.Name)
		}
	}
	sort.Strings(avail)
	return avail, nil
}

// ActiveBridges returns the enhance bridges live under the enabled set: a
// bridge fires only when its owning module and its When capability are both on.
func (r *Registry) ActiveBridges(enabled []string) []string {
	on := set(enabled)
	var active []string
	for name := range on {
		m := r.modules[name]
		if m == nil {
			continue
		}
		for _, b := range m.Enhances {
			if r.capabilityCovered(b.When, on) {
				active = append(active, m.Module+":"+b.Adds)
			}
		}
	}
	sort.Strings(active)
	return active
}

// capabilityCovered reports whether some module in the on-set provides cap.
func (r *Registry) capabilityCovered(cap string, on map[string]bool) bool {
	prov, ok := r.providers[cap]
	return ok && on[prov]
}

func (r *Registry) title(module string) string {
	if m, ok := r.modules[module]; ok && m.Title != "" {
		return m.Title
	}
	return module
}

func set(names []string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
