// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/modules/ledger"
)

// A pack is data, so the things that would otherwise be caught by a reviewer
// squinting at YAML are caught here instead.

func TestShippedPacks(t *testing.T) {
	packs := Dashboards()
	if len(packs) < 3 {
		t.Fatalf("got %d dashboard packs, want the CEO/CFO/AR set at least", len(packs))
	}

	seen := map[string]bool{}
	for _, d := range packs {
		t.Run(d.Name, func(t *testing.T) {
			if seen[d.Name] {
				t.Fatalf("duplicate pack name %q", d.Name)
			}
			seen[d.Name] = true

			if d.Title == "" {
				t.Error("pack has no title")
			}
			if len(d.Roles) == 0 {
				t.Error("pack suggests no roles, so it can never be anyone's default")
			}
			if d.Headline == "" {
				t.Fatal("pack has no headline — docs/21 §3 wants a dominant top-left tile")
			}
			for _, tile := range d.Tiles {
				if tile == d.Headline {
					t.Errorf("tile %q repeats the headline", tile)
				}
			}
		})
	}
}

// A pack that needs no capability must reference only metrics this build has:
// a tile pointing at a metric nobody registered renders as a silent omission,
// which looks exactly like a permission problem to whoever is staring at it.
//
// A pack that declares `requires:` is exempt, because its metrics belong to a
// module that has not shipped yet — that is the whole point of gating it
// (WP-1.8-decisions.md §4).
func TestPackMetricsExistUnlessGated(t *testing.T) {
	for _, d := range Dashboards() {
		if len(d.Requires) > 0 {
			continue
		}
		for _, name := range d.metricNames() {
			if _, err := lookup(name); err != nil {
				t.Errorf("pack %q references unknown metric %q (and declares no requires:)", d.Name, name)
			}
		}
	}
}

// The gated pack is the AP one, and it must stay gated until payables exists —
// otherwise it would list as available and render nothing.
func TestGatedPackIsNotRenderable(t *testing.T) {
	var gated []Dashboard
	for _, d := range Dashboards() {
		if len(d.Requires) > 0 {
			gated = append(gated, d)
		}
	}
	if len(gated) == 0 {
		t.Skip("no gated packs ship")
	}
	for _, d := range gated {
		if _, err := lookup(d.Headline); err == nil {
			t.Errorf("pack %q declares requires:%v but its headline metric %q already exists — either drop the gate or the pack is live",
				d.Name, d.Requires, d.Headline)
		}
	}
}

// Headline first is what makes the rendered order the reading order.
func TestMetricNamesLeadWithTheHeadline(t *testing.T) {
	for _, d := range Dashboards() {
		names := d.metricNames()
		if len(names) == 0 || names[0] != d.Headline {
			t.Errorf("pack %q: metricNames = %v, want the headline first", d.Name, names)
		}
	}
}

// Every metric a pack can render declares a grain, since a comparison without
// one is a number compared to something of a different kind.
func TestEveryMetricDeclaresAGrain(t *testing.T) {
	for _, m := range Metrics() {
		if m.Grain != GrainFlow && m.Grain != GrainStock {
			t.Errorf("metric %q has grain %q, want flow or stock", m.Name, m.Grain)
		}
	}
}

// Flow metrics measure movement, so they must be income-statement measures;
// stock metrics are balances. This is a spot-check that the declarations match
// what the evaluators actually read.
func TestGrainsMatchTheMeasure(t *testing.T) {
	want := map[string]Grain{
		"revenue": GrainFlow, "expenses": GrainFlow, "net_income": GrainFlow,
		"total_assets": GrainStock, "total_liabilities": GrainStock,
		"cash_position": GrainStock, "ar_outstanding": GrainStock,
		"ar_overdue": GrainStock, "open_invoice_count": GrainStock,
	}
	for name, grain := range want {
		m, err := lookup(name)
		if err != nil {
			t.Errorf("metric %q is gone: %v", name, err)
			continue
		}
		if m.Grain != grain {
			t.Errorf("metric %q has grain %q, want %q", name, m.Grain, grain)
		}
	}
}

// periodFilter is the difference between "revenue this month" and "revenue ever".
func TestPeriodFilter(t *testing.T) {
	periods := testPeriods("2026-05", "2026-06", "2026-07")

	flow, err := periodFilter(periods, "2026-06", GrainFlow)
	if err != nil {
		t.Fatalf("periodFilter flow: %v", err)
	}
	for period, want := range map[string]bool{"2026-05": false, "2026-06": true, "2026-07": false} {
		if got := flow(period); got != want {
			t.Errorf("flow filter for %s = %t, want %t", period, got, want)
		}
	}

	stock, err := periodFilter(periods, "2026-06", GrainStock)
	if err != nil {
		t.Fatalf("periodFilter stock: %v", err)
	}
	for period, want := range map[string]bool{"2026-05": true, "2026-06": true, "2026-07": false} {
		if got := stock(period); got != want {
			t.Errorf("stock filter for %s = %t, want %t", period, got, want)
		}
	}

	// An entry whose period record is gone cannot be placed in time: it counts
	// towards balances (the money is there) and towards no period's movement.
	if !stock("archived-period") {
		t.Error("stock filter dropped an entry in an unknown period, understating the balance")
	}
	if flow("archived-period") {
		t.Error("flow filter attributed an entry to a period it cannot place")
	}

	if _, err := periodFilter(periods, "2027-01", GrainStock); err == nil {
		t.Error("periodFilter accepted a period the tenant does not have")
	} else if !strings.Contains(err.Error(), "unknown period") {
		t.Errorf("unexpected error for an unknown period: %v", err)
	}
}

// testPeriods builds an ordered period list from codes.
func testPeriods(codes ...string) []ledger.Period {
	out := make([]ledger.Period, 0, len(codes))
	for _, c := range codes {
		out = append(out, ledger.Period{Code: c, StartDate: c + "-01", EndDate: c + "-28"})
	}
	return out
}

// The bootstrapped administrator holds one role, so exactly one pack may claim
// it — otherwise "their role's dashboard" resolves to whichever pack sorts
// first, which is how an owner-operator ends up staring at the AR clerk's view.
func TestOnlyOnePackClaimsTheAdministratorRole(t *testing.T) {
	var claimants []string
	for _, d := range Dashboards() {
		for _, role := range d.Roles {
			if role == "administrator" {
				claimants = append(claimants, d.Name)
			}
		}
	}
	if len(claimants) != 1 {
		t.Errorf("packs claiming the administrator role = %v, want exactly one", claimants)
	}
}
