// SPDX-License-Identifier: AGPL-3.0-only

package capability

import "sort"

// Profile is a curated enable-set (ADR-018 §6). WP-0.9 ships the definitions;
// seed data + role packs land with WP-1.8. A profile is only a starting point
// — every module stays user-flippable afterward.
type Profile struct {
	// Name is the stable profile identifier.
	Name string
	// Title is the human label.
	Title string
	// Modules are the modules the profile turns on (their closure is applied).
	Modules []string
	// Pending, if non-empty, names a capability the profile needs that no
	// shipped module provides yet (e.g. documents.ocr for capture→export). A
	// pending profile is defined but not bootable until its provider lands.
	Pending string
}

// Bootable reports whether the profile can be applied now.
func (p Profile) Bootable() bool { return p.Pending == "" }

// profiles is the shipped skeleton set (ADR-018 §6). Invoice-automation-only
// is defined but Pending documents.ocr (its capture→export mode needs OCR,
// which lands with M3) — see WP-0.9 decisions §7.
var profiles = []Profile{
	{Name: "personal", Title: "Personal", Modules: []string{"invoicing", "payables"}},
	{Name: "accounting_only", Title: "Accounting-only", Modules: []string{"ledger", "invoicing", "payables", "banking"}},
	{Name: "crm_only", Title: "CRM-only", Modules: []string{"crm"}},
	{Name: "invoice_automation_only", Title: "Invoice-automation-only", Modules: []string{"payables"}, Pending: "documents.ocr"},
	{Name: "services_smb", Title: "Services SMB", Modules: []string{"invoicing", "payables", "banking", "crm", "projects", "work"}},
	{Name: "product_smb", Title: "Product SMB", Modules: []string{"invoicing", "payables", "banking", "crm", "projects", "inventory", "work"}},
	{Name: "full_suite", Title: "Full suite", Modules: []string{
		"ledger", "invoicing", "payables", "banking", "crm", "inventory", "hr", "payroll", "projects", "work", "tax-engine", "contacts",
	}},
}

// Profiles returns the shipped profiles.
func Profiles() []Profile {
	out := make([]Profile, len(profiles))
	copy(out, profiles)
	return out
}

// Profile returns a profile by name.
func (r *Registry) Profile(name string) (Profile, bool) {
	for _, p := range profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// ProfileClosure resolves a profile to the full sorted module set it enables.
func (r *Registry) ProfileClosure(p Profile) ([]string, error) {
	mods := append([]string{}, p.Modules...)
	sort.Strings(mods)
	return r.Closure(mods)
}
