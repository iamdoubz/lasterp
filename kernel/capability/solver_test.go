//go:build integrity

package capability

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func mustLoad(t *testing.T) *Registry {
	t.Helper()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

// The catalog loads and every docs/10 module is present with a closed graph
// (Load fails on a dangling base requires — validateGraph). This is the
// "declare a manifest or fail CI" gate in skeleton form.
func TestManifestCatalogComplete(t *testing.T) {
	r := mustLoad(t)
	want := []string{
		"kernel", "contacts", "tax-engine", "ledger", "invoicing", "payables",
		"banking", "crm", "inventory", "hr", "payroll", "projects", "work",
	}
	for _, name := range want {
		if _, err := r.Module(name); err != nil {
			t.Errorf("missing manifest for %q: %v", name, err)
		}
	}
}

// ADR-018 §3: enabling a module shows the full closure before applying.
func TestEnableClosurePreview(t *testing.T) {
	r := mustLoad(t)
	res, err := r.EnableClosure(nil, "invoicing")
	if err != nil {
		t.Fatalf("EnableClosure: %v", err)
	}
	for _, want := range []string{"invoicing", "contacts", "ledger", "tax-engine"} {
		if !slices.Contains(res.Added, want) {
			t.Errorf("closure of invoicing missing %q; got %v", want, res.Added)
		}
	}
	preview := r.Preview(res)
	if !strings.Contains(preview, "Ledger") || !strings.Contains(preview, "Tax engine") {
		t.Errorf("preview does not name the dependencies: %q", preview)
	}
}

func TestClosureDeterministic(t *testing.T) {
	r := mustLoad(t)
	a, _ := r.Closure([]string{"invoicing", "banking"})
	b, _ := r.Closure([]string{"banking", "invoicing"})
	if !slices.Equal(a, b) {
		t.Errorf("closure not deterministic: %v vs %v", a, b)
	}
}

// ADR-018 §3: disabling checks reverse dependencies.
func TestDisableImpactReverseDep(t *testing.T) {
	r := mustLoad(t)
	enabled, _ := r.Closure([]string{"invoicing"}) // pulls in ledger, contacts, tax-engine
	broken, err := r.DisableImpact(enabled, "ledger")
	if err != nil {
		t.Fatalf("DisableImpact: %v", err)
	}
	if !slices.Contains(broken, "invoicing") {
		t.Errorf("disabling ledger should break invoicing; got %v", broken)
	}
	// A leaf with no dependents disables cleanly.
	if b, _ := r.DisableImpact(enabled, "invoicing"); len(b) != 0 {
		t.Errorf("disabling leaf invoicing should break nothing; got %v", b)
	}
}

func TestDisableKernelRefused(t *testing.T) {
	r := mustLoad(t)
	if _, err := r.DisableImpact([]string{"kernel"}, "kernel"); !errors.Is(err, ErrKernelNotDisableable) {
		t.Errorf("disabling kernel: got %v, want ErrKernelNotDisableable", err)
	}
}

// ADR-018 §4 / WP-0.9 decisions §7: a reduced mode whose extra requires have
// no provider (documents.ocr before M3) is reported unavailable; a mode with
// satisfiable requires is available.
func TestReducedModeAvailability(t *testing.T) {
	r := mustLoad(t)
	enabled, _ := r.Closure([]string{"payables"})
	modes, err := r.AvailableModes(enabled, "payables")
	if err != nil {
		t.Fatalf("AvailableModes: %v", err)
	}
	if slices.Contains(modes, "capture_export") {
		t.Errorf("capture_export needs documents.ocr (unprovided) and must be unavailable; got %v", modes)
	}
	bankModes, _ := r.AvailableModes([]string{"banking"}, "banking")
	if !slices.Contains(bankModes, "statement_import") {
		t.Errorf("statement_import has no extra requires and should be available; got %v", bankModes)
	}
}

// Enhance bridge activates only when both sides are on (ADR-018 §2).
func TestEnhanceBridgeActivation(t *testing.T) {
	r := mustLoad(t)
	if got := r.ActiveBridges([]string{"inventory"}); slices.Contains(got, "inventory:stock-valuation") {
		t.Errorf("inventory alone should not activate stock-valuation; got %v", got)
	}
	enabled, _ := r.Closure([]string{"inventory", "ledger"})
	if got := r.ActiveBridges(enabled); !slices.Contains(got, "inventory:stock-valuation") {
		t.Errorf("inventory+ledger should activate stock-valuation; got %v", got)
	}
}

// ADR-018 §7 integrity floor: any money module drags in ledger.core.
func TestMoneyModulesRequireLedger(t *testing.T) {
	r := mustLoad(t)
	for _, money := range []string{"invoicing", "payables", "banking", "payroll"} {
		closure, err := r.Closure([]string{money})
		if err != nil {
			t.Fatalf("closure %s: %v", money, err)
		}
		if !slices.Contains(closure, "ledger") {
			t.Errorf("%s closure must include ledger (INV-F5 floor); got %v", money, closure)
		}
	}
}

// Every bootable profile resolves to a satisfiable, deterministic closure that
// respects the money floor — the solver-level half of "every shipped profile
// boots" (the DB half is TestApplyProfileBootsEveryProfile).
func TestBootableProfilesResolve(t *testing.T) {
	r := mustLoad(t)
	bootable := 0
	for _, p := range Profiles() {
		if !p.Bootable() {
			continue
		}
		bootable++
		closure, err := r.ProfileClosure(p)
		if err != nil {
			t.Errorf("profile %s does not resolve: %v", p.Name, err)
			continue
		}
		if !slices.Contains(closure, "kernel") {
			t.Errorf("profile %s closure omits kernel: %v", p.Name, closure)
		}
		for _, money := range []string{"invoicing", "payables", "banking", "payroll"} {
			if slices.Contains(closure, money) && !slices.Contains(closure, "ledger") {
				t.Errorf("profile %s enables %s without ledger", p.Name, money)
			}
		}
	}
	if bootable != 6 {
		t.Errorf("expected 6 bootable profiles, got %d", bootable)
	}
}

func TestInvoiceAutomationProfilePending(t *testing.T) {
	r := mustLoad(t)
	p, ok := r.Profile("invoice_automation_only")
	if !ok {
		t.Fatal("invoice_automation_only profile missing")
	}
	if p.Bootable() {
		t.Error("invoice_automation_only should be pending documents.ocr, not bootable")
	}
}
