// SPDX-License-Identifier: AGPL-3.0-only

package capability

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// Registry is the loaded capability catalog: every module's manifest plus the
// derived capability→module and object→module indexes. It is immutable after
// Load and safe to share.
type Registry struct {
	modules   map[string]*Manifest // by module name
	providers map[string]string    // capability -> providing module name
	objects   map[string]string    // metadata object name -> owning module name
	kernel    []string             // always-on module names
}

// ErrUnknownModule is returned for a module name not in the catalog.
var ErrUnknownModule = errors.New("capability: unknown module")

// Load builds the Registry from the embedded built-in catalog (docs/10).
func Load() (*Registry, error) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("capability: read builtin dir: %w", err)
	}
	r := &Registry{
		modules:   map[string]*Manifest{},
		providers: map[string]string{},
		objects:   map[string]string{},
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := fs.ReadFile(builtinFS, "builtin/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("capability: read %s: %w", e.Name(), err)
		}
		m, err := ParseManifest(data)
		if err != nil {
			return nil, fmt.Errorf("capability: %s: %w", e.Name(), err)
		}
		if err := r.add(m); err != nil {
			return nil, err
		}
	}
	if err := r.validateGraph(); err != nil {
		return nil, err
	}
	sort.Strings(r.kernel)
	return r, nil
}

func (r *Registry) add(m *Manifest) error {
	if _, dup := r.modules[m.Module]; dup {
		return fmt.Errorf("capability: duplicate module %q", m.Module)
	}
	r.modules[m.Module] = m
	if m.Kernel {
		r.kernel = append(r.kernel, m.Module)
	}
	for _, cap := range m.Provides {
		if owner, dup := r.providers[cap]; dup {
			return fmt.Errorf("capability: %q provided by both %q and %q", cap, owner, m.Module)
		}
		r.providers[cap] = m.Module
	}
	for _, obj := range m.Objects {
		if owner, dup := r.objects[obj]; dup {
			return fmt.Errorf("capability: object %q owned by both %q and %q", obj, owner, m.Module)
		}
		r.objects[obj] = m.Module
	}
	return nil
}

// validateGraph ensures every base requires resolves to a provider — a closed
// graph is what lets profiles boot. Mode requires are deliberately NOT checked
// here: a reduced mode may name a capability a future module will provide
// (WP-0.9 decisions §7); availableModes gates those at enable time.
func (r *Registry) validateGraph() error {
	for _, m := range r.modules {
		for _, cap := range m.Requires {
			if _, ok := r.providers[cap]; !ok {
				return fmt.Errorf("capability: module %q requires %q which no module provides", m.Module, cap)
			}
		}
	}
	return nil
}

// Module returns a manifest by name.
func (r *Registry) Module(name string) (*Manifest, error) {
	m, ok := r.modules[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModule, name)
	}
	return m, nil
}

// Modules returns every module name, sorted.
func (r *Registry) Modules() []string {
	names := make([]string, 0, len(r.modules))
	for name := range r.modules {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ModuleForObject returns the module owning a metadata object and whether any
// module claims it. Objects with no owner are kernel objects — always allowed.
func (r *Registry) ModuleForObject(object string) (string, bool) {
	m, ok := r.objects[object]
	return m, ok
}
