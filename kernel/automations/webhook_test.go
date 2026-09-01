// SPDX-License-Identifier: AGPL-3.0-only

package automations

import (
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/outbound"
)

// WP-3.3c, the parse half. What a `webhook` action may say, and what it may
// not: a destination id and literals, never a URL and never an expression
// (docs/notes/WP-3.3c-decisions.md §2-3).

const webhookYAML = `
id: notify-ops
name: Notify ops on a big invoice
trigger:
  object: Invoice
condition: 'record.total_minor > 100000'
actions:
  - type: webhook
    destination: ops-slack
    body:
      severity: high
      urgent: true
`

func TestParseWebhookAction(t *testing.T) {
	d, err := Parse([]byte(webhookYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := d.Actions[0].Destination; got != "ops-slack" {
		t.Fatalf("destination = %q", got)
	}
	// The authority a webhook needs is derived from the definition, not
	// declared beside it: `Webhook:send` is what Save then bounds against the
	// creator's own grants (INV-T3).
	var found bool
	for _, p := range d.Permissions() {
		if p[0] == outbound.ObjectWebhook && p[1] == outbound.ActionSend {
			found = true
		}
	}
	if !found {
		t.Fatalf("a webhook action did not require %s:%s — permissions = %v",
			outbound.ObjectWebhook, outbound.ActionSend, d.Permissions())
	}
	// It must *not* pick up the power to register destinations. Deciding where
	// the server may call out is the other key, and an automation never holds
	// it (§1).
	for _, p := range d.Permissions() {
		if p[0] == outbound.ObjectWebhook && p[1] == outbound.ActionManage {
			t.Fatal("a webhook action asked for Webhook:manage — an automation may use a destination, never register one")
		}
	}
}

// A URL in the definition is the thing this design exists to prevent: the
// document is handed to anyone holding `Automation:manage`, a webhook's path is
// itself a credential, and a rule that names its own host is a rule whose
// author chose where the server calls out.
func TestParseWebhookRefusesAnythingButADestinationID(t *testing.T) {
	for name, dest := range map[string]string{
		"a url":       "https://hooks.slack.com/services/T0/B0/secret",
		"empty":       "",
		"uppercase":   "Ops-Slack",
		"traversal":   "../other",
		"with spaces": "ops slack",
	} {
		t.Run(name, func(t *testing.T) {
			src := "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - type: webhook\n    destination: " +
				`"` + dest + `"` + "\n"
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatalf("destination %q was accepted", dest)
			}
		})
	}
	// A bare hostname is *not* refused here, and deliberately: `ops.slack` is a
	// perfectly good id and no shape rule separates it from `hooks.slack.com`.
	// The bound that matters is the registry — Save refuses a destination this
	// tenant never registered (TestSaveRefusesAnUnregisteredDestination), which
	// is a fact rather than a heuristic.
	//
	// Non-vacuity: a plain id is accepted, or the checks above prove only that
	// the parser refuses everything.
	src := "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - type: webhook\n    destination: ops-slack\n"
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("a plain destination id was refused: %v", err)
	}
}

// The body is literals merged into a fixed envelope. No expression (a second
// evaluation surface), no nesting (its first step), and no overriding what the
// engine states about the firing.
func TestParseWebhookBodyIsLiteralsOnly(t *testing.T) {
	for name, body := range map[string]string{
		"nested map":       "      detail: {a: 1}\n",
		"list":             "      detail: [1, 2]\n",
		"envelope key":     "      record_id: forged\n",
		"envelope key at":  "      at: 1999-01-01T00:00:00Z\n",
		"envelope key obj": "      object: Contact\n",
	} {
		t.Run(name, func(t *testing.T) {
			src := "id: a\nname: A\ntrigger:\n  object: Invoice\nactions:\n  - type: webhook\n    destination: ops\n    body:\n" + body
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("body %q was accepted", body)
			}
			if strings.Contains(name, "envelope") && !strings.Contains(err.Error(), "override") {
				t.Fatalf("the refusal does not say why: %v", err)
			}
		})
	}
	if _, err := Parse([]byte(webhookYAML)); err != nil {
		t.Fatalf("a literal body was refused: %v", err)
	}
}

// A webhook works from either trigger — unlike field_update, it needs no record
// to write to. A scheduled one sends an envelope with an empty record_id.
func TestWebhookBodyEnvelope(t *testing.T) {
	d, err := Parse([]byte(webhookYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := webhookBody(d, &d.Actions[0], "inv-1")
	for key, want := range map[string]any{
		"automation": "notify-ops", "object": "Invoice", "record_id": "inv-1",
		"severity": "high", "urgent": true,
	} {
		if got[key] != want {
			t.Errorf("body[%q] = %v, want %v", key, got[key], want)
		}
	}
	if got["at"] == "" || got["at"] == nil {
		t.Error("the envelope carries no timestamp")
	}
	// No record fields. Which fields of a record may leave the tenant is a
	// data-egress policy nobody has designed, so the receiver gets an id and
	// has an API (§3).
	if len(got) != 6 {
		t.Fatalf("the body carries more than the envelope and the literals: %v", got)
	}

	schedule := "id: s\nname: S\ntrigger:\n  schedule: '0 2 * * *'\nactions:\n  - type: webhook\n    destination: ops\n"
	if _, err := Parse([]byte(schedule)); err != nil {
		t.Fatalf("a scheduled webhook was refused: %v", err)
	}
}
