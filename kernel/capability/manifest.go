// SPDX-License-Identifier: AGPL-3.0-only

// Package capability is the WP-0.9 composability kernel (ADR-018): each
// module declares a capability manifest (provides/requires/enhances/modes),
// the registry loads the built-in catalog, and the solver computes the
// dependency closure the UI shows before enabling. Enable-state is
// per-tenant and disable never deletes data.
package capability

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Manifest is one module's capability declaration (ADR-018 §2). Capabilities
// are finer-grained strings than modules (e.g. "contacts", "ledger.core") so
// a light dependency doesn't drag a whole module in.
type Manifest struct {
	// Module is the unique module name (matches the file stem in builtin/).
	Module string `yaml:"module"`
	// Title is the human label shown in previews.
	Title string `yaml:"title"`
	// Kernel marks the always-on, non-disableable kernel (ADR-018 §1).
	Kernel bool `yaml:"kernel"`
	// Provides lists the capabilities this module offers.
	Provides []string `yaml:"provides"`
	// Requires lists capabilities that must be enabled for this module to run
	// (hard deps). Every entry must resolve to a provider in the catalog.
	Requires []string `yaml:"requires"`
	// Enhances are Odoo-style bridges: extra behaviour that auto-activates
	// only when the named capability is also enabled — never a hard dep.
	Enhances []Bridge `yaml:"enhances"`
	// Modes are designed reduced configurations (ADR-018 §4). A mode may
	// require capabilities not yet provided by any shipped module; the solver
	// reports such a mode unavailable until its provider exists.
	Modes []Mode `yaml:"modes"`
	// Objects lists the metadata object names this module owns, so the API
	// gateway can gate them behind the module's enable-state. Optional.
	Objects []string `yaml:"objects"`
}

// Bridge is an enhance edge: when When is enabled, Adds activates.
type Bridge struct {
	When string `yaml:"when"`
	Adds string `yaml:"adds"`
}

// Mode is a reduced configuration with its own extra requires.
type Mode struct {
	Name     string   `yaml:"name"`
	Requires []string `yaml:"requires"`
}

// ParseManifest parses and validates one manifest's YAML.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("capability: parse manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Module == "" {
		return fmt.Errorf("capability: manifest has no module name")
	}
	if !m.Kernel && len(m.Provides) == 0 {
		return fmt.Errorf("capability: module %q provides nothing (only the kernel may)", m.Module)
	}
	for _, mode := range m.Modes {
		if mode.Name == "" {
			return fmt.Errorf("capability: module %q has an unnamed mode", m.Module)
		}
	}
	return nil
}

// primaryCapability is the capability the gateway reports when an object of
// this module is accessed while disabled. Empty for the kernel.
func (m *Manifest) primaryCapability() string {
	if len(m.Provides) == 0 {
		return ""
	}
	return m.Provides[0]
}
