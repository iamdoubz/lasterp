// SPDX-License-Identifier: AGPL-3.0-only

package automations

import (
	"errors"
	"strings"
	"testing"
)

const validYAML = `
id: nudge-overdue
name: Nudge overdue invoices
trigger:
  object: Invoice
  on: [update]
condition: 'record.status == "posted" && record.total_minor > 100000'
actions:
  - type: field_update
    set:
      followup: true
`

func TestParseValid(t *testing.T) {
	d, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.ID != "nudge-overdue" || d.TriggerKind() != TriggerObject {
		t.Fatalf("parsed = %+v", d)
	}
	if d.Principal() != "automation:nudge-overdue" {
		t.Fatalf("principal = %q", d.Principal())
	}
	if !d.IsEnabled() {
		t.Fatal("an automation with no `enabled:` should be enabled")
	}
}

// docs/05's action table lists email, webhook and approval_request. `webhook`
// landed in WP-3.3c; the other two are not built. Each is refused at parse time naming its owner, never accepted and
// silently skipped — the rule the plugin manifest already follows, because an
// administrator who writes one and gets a no-op has been lied to.
func TestParseRefusesDeferredActionsByName(t *testing.T) {
	for action, wantOwner := range map[string]string{
		"email":            "outbound mail",
		"approval_request": "WP-3.4",
	} {
		t.Run(action, func(t *testing.T) {
			src := "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - type: " + action + "\n"
			_, err := Parse([]byte(src))
			if !errors.Is(err, ErrDeferredAction) {
				t.Fatalf("Parse(%s): err = %v, want ErrDeferredAction", action, err)
			}
			if !strings.Contains(err.Error(), wantOwner) {
				t.Fatalf("refusal for %s does not name its owner: %v", action, err)
			}
		})
	}
}

func TestParseRefuses(t *testing.T) {
	cases := map[string]string{
		"no id":            "name: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"bad id":           "id: Not Valid!\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"no name":          "id: a\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"no trigger":       "id: a\nname: A\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"two triggers":     "id: a\nname: A\ntrigger:\n  object: Invoice\n  schedule: \"0 2 * * *\"\nactions:\n  - {type: call_plugin, plugin: p, fn: f}\n",
		"no actions":       "id: a\nname: A\ntrigger:\n  object: Invoice\n",
		"unknown action":   "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: teleport}\n",
		"unknown key":      "id: a\nname: A\ntrigger:\n  object: Invoice\nsuperpowers: true\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"bad verb":         "id: a\nname: A\ntrigger:\n  object: Invoice\n  on: [explode]\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"bad cron":         "id: a\nname: A\ntrigger:\n  schedule: \"0 2 * *\"\nactions:\n  - {type: call_plugin, plugin: p, fn: f}\n",
		"unfiring cron":    "id: a\nname: A\ntrigger:\n  schedule: \"0 0 30 2 *\"\nactions:\n  - {type: call_plugin, plugin: p, fn: f}\n",
		"bad condition":    "id: a\nname: A\ntrigger:\n  object: Invoice\ncondition: 'record.x =='\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"non-bool cond":    "id: a\nname: A\ntrigger:\n  object: Invoice\ncondition: 'record.x'\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		"cond outside env": "id: a\nname: A\ntrigger:\n  object: Invoice\ncondition: 'secrets.get(\"k\") != \"\"'\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		// A field_update needs a record; a schedule trigger has none.
		"update on schedule": "id: a\nname: A\ntrigger:\n  schedule: \"0 2 * * *\"\nactions:\n  - {type: field_update, set: {x: 1}}\n",
		// id and tenant_id are not an automation's to move.
		"sets id":          "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {id: x}}\n",
		"sets tenant_id":   "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {tenant_id: x}}\n",
		"empty set":        "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {}}\n",
		"plugin no fn":     "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - {type: call_plugin, plugin: p}\n",
		"on with schedule": "id: a\nname: A\ntrigger:\n  schedule: \"0 2 * * *\"\n  on: [create]\nactions:\n  - {type: call_plugin, plugin: p, fn: f}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Error("accepted a definition this engine cannot honour exactly")
			}
		})
	}
}

// The condition is compiled at write time in the same closed environment an
// RBAC grant uses (ADR-022) — so a mistyped rule fails where its author can see
// it, rather than becoming an automation that silently never fires.
func TestConditionUsesTheSharedEnvironment(t *testing.T) {
	src := "id: a\nname: A\ntrigger:\n  object: Invoice\ncondition: 'record.owner == actor.id'\nactions:\n  - {type: field_update, set: {x: 1}}\n"
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("Parse with a record/actor condition: %v", err)
	}
}

func TestScheduleTriggerParses(t *testing.T) {
	src := "id: nightly\nname: Nightly\ntrigger:\n  schedule: \"0 2 * * *\"\nactions:\n  - {type: call_plugin, plugin: com.acme.x, fn: run}\n"
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.TriggerKind() != TriggerSchedule {
		t.Fatalf("trigger kind = %q, want schedule", d.TriggerKind())
	}
}

func TestDisabledIsExplicit(t *testing.T) {
	src := "id: a\nname: A\nenabled: false\ntrigger:\n  object: Invoice\nactions:\n  - {type: field_update, set: {x: 1}}\n"
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.IsEnabled() {
		t.Fatal("enabled: false parsed as enabled")
	}
}
