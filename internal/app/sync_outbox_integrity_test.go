//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// WP-2.3b: the upstream half of sync — the outbox, the drain, and the two
// server-side affordances they rest on.
//
// The end-to-end properties (a command survives a crash mid-drain, every
// command reaches exactly one terminal state) live in the simulation harness,
// where there is a real replica to lose work in. This file holds the parts that
// are provable against the server alone:
//
//   - **the client-supplied row id** (WP-2.3-decisions.md §2), without which an
//     optimistically-applied create has no final id until acceptance;
//   - **INV-S2 structurally** — that there is no sync write endpoint for a
//     drain to prefer over the ordinary one.

// --- the drain, end to end (INV-S1, INV-S4) ---

// The plain case first, because everything below is a variation on it: work
// done while disconnected reaches the server, the queue empties, and the
// replica ends up agreeing with the server about all of it.
func TestOfflineWorkReachesTheServerOnReconnect(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before going offline")

			const queued = 5
			dump := run.queueOffline("Offline", queued, 0)
			if got := len(dump["_outbox"]); got != queued {
				t.Fatalf("%d commands queued, want %d — the outbox did not accept the work", got, queued)
			}
			// docs/04 §Upstream 1: applied optimistically, so the user can see
			// what they just did. A queue the work vanishes into is not offline
			// support, it is a delayed save.
			if got := countNamed(t, e, dump, "Contact", "Offline "); got != queued {
				t.Fatalf("%d optimistic rows in the replica, want %d", got, queued)
			}
			if onServer := serverCountNamed(t, e, "Offline "); onServer != 0 {
				t.Fatalf("%d rows reached the server while offline, want 0", onServer)
			}

			dump = run.sync(0)
			if got := len(dump["_outbox"]); got != 0 {
				t.Fatalf("%d commands left queued after a clean drain, want 0", got)
			}
			if got := len(dump["_conflicts"]); got != 0 {
				t.Fatalf("%d conflicts filed for work the server accepted: %v", got, dump["_conflicts"])
			}
			if onServer := serverCountNamed(t, e, "Offline "); onServer != queued {
				t.Fatalf("%d rows on the server, want %d — offline work was lost in the drain "+
					"(INV-S1)", onServer, queued)
			}
			assertConverged(t, e, run.sync(0), "after draining offline work")
		})
	}
}

// **INV-S1, the crash that matters.** The window is one instruction wide: the
// server has committed, the client has the 201 in hand, and the record of
// having sent it is not yet durable. A process that dies exactly there must, on
// the next drain, re-send and be deduplicated (INV-E4) — never lose the write,
// never apply it twice.
//
// Nothing else in the suite reaches this state. A partition cuts before the
// server sees anything; a crash mid-apply is downstream. This is the only test
// that can tell RPO 0 from "usually fine".
func TestCrashBetweenAcceptanceAndAcknowledgementLosesNothing(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before going offline")

			const queued = 4
			run.queueOffline("Crashed", queued, 0)

			// Die immediately after the first command's response is read.
			out := run.syncFaulted("--kill-after-command=1")
			if out.err != nil {
				t.Fatal(out.err)
			}
			if !out.crashed {
				t.Fatalf("the driver was told to die after its first command and did not — "+
					"this test proves nothing about the acknowledgement window\nstderr: %s",
					out.stderr)
			}

			// The server took that first command. The client does not know.
			if onServer := serverCountNamed(t, e, "Crashed "); onServer != 1 {
				t.Fatalf("%d rows on the server after one accepted command, want 1", onServer)
			}

			dump := run.sync(0)
			if got := len(dump["_outbox"]); got != 0 {
				t.Fatalf("%d commands left queued after recovery, want 0 — work the user did "+
					"offline is stuck (INV-S1)", got)
			}
			if got := len(dump["_conflicts"]); got != 0 {
				t.Fatalf("recovery filed %d conflicts: %v — a re-sent command was mistaken for "+
					"a rejection", got, dump["_conflicts"])
			}
			// Not queued+1: the re-sent command must be deduplicated by its
			// Idempotency-Key rather than creating a second row.
			if onServer := serverCountNamed(t, e, "Crashed "); onServer != queued {
				t.Fatalf("%d rows on the server, want %d — a crash between acceptance and "+
					"acknowledgement %s (INV-S1/INV-E4)", onServer, queued,
					map[bool]string{true: "duplicated a write", false: "lost a write"}[onServer > queued])
			}
			assertConverged(t, e, run.sync(0), "after recovering from the acknowledgement crash")
		})
	}
}

// **INV-S4: no silent drops.** The property is conservation. Every command that
// enters the outbox leaves it in exactly one of two ways — accepted by the
// server, or filed where a person can see it — and the counts add up. A command
// that is neither is the failure this invariant names, and it is invisible
// without counting.
func TestEveryCommandReachesExactlyOneTerminalState(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before going offline")

			const valid, invalid = 4, 1
			run.queueOffline("Mixed", valid, invalid)

			// Stop-on-reject (WP-2.3-decisions.md §9) means one drain may not
			// finish the queue: the rejection halts it and the rest wait. Drain
			// until it settles, which is what a user reconnecting repeatedly
			// does anyway.
			var dump map[string][]map[string]any
			for i := 0; i < valid+invalid+1; i++ {
				dump = run.sync(0)
				if len(dump["_outbox"]) == 0 {
					break
				}
			}

			accepted := serverCountNamed(t, e, "Mixed ")
			filed := len(dump["_conflicts"])
			stillQueued := len(dump["_outbox"])

			if stillQueued != 0 {
				t.Fatalf("%d commands never settled: %v", stillQueued, dump["_outbox"])
			}
			if accepted+filed != valid+invalid {
				t.Fatalf("%d accepted + %d filed = %d, want %d — a command left the outbox "+
					"without reaching either terminal state, which is the silent drop INV-S4 "+
					"forbids", accepted, filed, accepted+filed, valid+invalid)
			}
			if filed != invalid {
				t.Fatalf("%d conflicts filed, want %d: %v", filed, invalid, dump["_conflicts"])
			}
			if accepted != valid {
				t.Fatalf("%d rows accepted, want %d", accepted, valid)
			}
		})
	}
}

// A rejection is the server's own problem+json, and the optimistic row it
// created is gone. Both halves matter: the first is what makes the tray worth
// reading, and the second is what keeps a refused change from sitting in the
// replica looking like data (INV-S3).
func TestARejectedCommandIsFiledWithTheServersReason(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before going offline")

			run.queueOffline("Refused", 0, 1)
			dump := run.sync(0)

			filed := dump["_conflicts"]
			if len(filed) != 1 {
				t.Fatalf("%d conflicts filed, want 1 — a refused command was dropped silently "+
					"(INV-S4)", len(filed))
			}
			if status := fmt.Sprint(filed[0]["status"]); status != "422" {
				t.Fatalf("conflict status = %s, want 422", status)
			}
			if title, _ := filed[0]["title"].(string); title == "" {
				t.Fatal("the filed conflict carries no title — the tray would show a blank reason")
			}
			// The detail is the server's, so the user reads what actually
			// happened rather than a client-side guess at it.
			if detail, _ := filed[0]["detail"].(string); !strings.Contains(detail, "email") {
				t.Fatalf("conflict detail = %q, want the server's own explanation naming the "+
					"field it refused", detail)
			}

			if got := countNamed(t, e, dump, "Contact", "Refused "); got != 0 {
				t.Fatalf("%d refused rows are still in the replica — a rejected create must roll "+
					"back, or the replica holds a row the server never accepted", got)
			}
			assertConverged(t, e, run.sync(0), "after a rejection")
		})
	}
}

// **INV-S2, behaviourally.** The structural tests above say there is no second
// write path; this says the one path treats an offline command exactly as it
// treats an online request. Same body, same route, same answer — the drain gets
// no dispensation for having waited.
func TestADrainedCommandIsJudgedLikeAnOnlineOne(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before going offline")

			run.queueOffline("Judged", 0, 1)
			filed := run.sync(0)["_conflicts"]
			if len(filed) != 1 {
				t.Fatalf("%d conflicts filed, want 1", len(filed))
			}

			// The same body, sent online through the ordinary route.
			status, body, problem := e.post("/api/v1/contact", map[string]any{
				"name": "Judged online", "email": "not-an-address", "kind": "customer",
			})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("the same body online = %d, want 422; body=%s", status, body)
			}

			offlineTitle, _ := filed[0]["title"].(string)
			onlineTitle, _ := problem["title"].(string)
			if offlineTitle != onlineTitle {
				t.Fatalf("offline rejection title %q != online %q — the drain is being judged by "+
					"a different pipeline (INV-S2)", offlineTitle, onlineTitle)
			}
			offlineStatus := fmt.Sprint(filed[0]["status"])
			if offlineStatus != fmt.Sprint(http.StatusUnprocessableEntity) {
				t.Fatalf("offline rejection status %s != online 422 (INV-S2)", offlineStatus)
			}
		})
	}
}

// The green ticks above are worth exactly what this says they are.
//
// Same argument as TestSimulationHarnessDetectsADivergentClient: a suite that
// counts commands in and out could pass by construction if the driver never
// queued any. So take a run that must fail — work queued and a drain that is
// never allowed to happen — and require the accounting to notice.
func TestTheOutboxHarnessDetectsWorkThatNeverLeft(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before going offline")

			run.queueOffline("Stranded", 3, 0)

			// The wire is cut before the drain can send anything. Every command
			// must still be queued, the optimistic rows must still be there, and
			// convergence must *fail* — the replica holds three rows the server
			// has never heard of.
			out := run.syncFaulted("--fail-after=1")
			if out.err != nil {
				t.Fatal(out.err)
			}
			dump := run.syncWithArgs("--offline")
			if got := len(dump["_outbox"]); got != 3 {
				t.Fatalf("%d commands survived a partitioned drain, want 3 — work was discarded "+
					"rather than retried (INV-S1)", got)
			}
			if converged, _ := convergence(t, e, dump); converged {
				t.Fatal("a replica holding three rows the server has never seen still matched " +
					"the server projection — this harness cannot see optimistic state and its " +
					"INV-S1/S4 results must not be trusted")
			}
		})
	}
}

// --- helpers ---

// queueOffline runs the driver in enqueue-only mode: the user does some work,
// and nothing is sent. Returns the replica dump so a caller can assert on the
// optimistic state directly.
func (r *replicaRun) queueOffline(label string, valid, invalid int) map[string][]map[string]any {
	r.t.Helper()
	return r.syncWithArgs(
		"--offline",
		fmt.Sprintf("--label=%s", label),
		fmt.Sprintf("--enqueue=%d", valid),
		fmt.Sprintf("--enqueue-invalid=%d", invalid),
	)
}

// serverCountNamed counts the tenant's live contacts whose name starts with
// prefix, read through the ordinary API.
func serverCountNamed(t *testing.T, e *env, prefix string) int {
	t.Helper()
	status, body, parsed := e.get("/api/v1/contact")
	if status != http.StatusOK {
		t.Fatalf("list contacts = %d; body=%s", status, body)
	}
	rows, _ := parsed["data"].([]any)
	n := 0
	for _, raw := range rows {
		rec, _ := raw.(map[string]any)
		if name, _ := rec["name"].(string); strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

// countNamed counts rows in the replica dump whose name starts with prefix.
func countNamed(t *testing.T, _ *env, dump map[string][]map[string]any, object, prefix string) int {
	t.Helper()
	n := 0
	for _, row := range dump[object] {
		if name, _ := row["name"].(string); strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

// --- the client-supplied row id (§2) ---

// A client that applied a create optimistically is looking at a row under an
// id it chose. Honouring that id is what makes it the same row the server ends
// up with, rather than one the client has to rewrite every reference to.
func TestCreateHonoursAClientSuppliedID(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			want := idgen.New()
			status, body, rec := e.post("/api/v1/contact", map[string]any{
				"id": want, "name": "Chosen", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("create with a client id = %d; body=%s", status, body)
			}
			if got := mustField(t, rec, "id"); got != want {
				t.Fatalf("stored id = %s, want %s — an offline client's optimistic row "+
					"would not be the row the server has", got, want)
			}

			// And it is readable back under that id: the response echoing it
			// would prove nothing on its own.
			status, body, got := e.get("/api/v1/contact/" + want)
			if status != http.StatusOK {
				t.Fatalf("get %s = %d; body=%s", want, status, body)
			}
			if mustField(t, got, "name") != "Chosen" {
				t.Fatalf("read back %v, want the row created under the supplied id", got)
			}
		})
	}
}

// A collision is a 409 that says only that the id is taken. The primary key is
// global per table, so a caller who guessed another tenant's row id must not be
// able to learn it exists (INV-T1) — which is a property of the *detail*, not
// just of the status.
func TestCreateRefusesADuplicateID(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			id := idgen.New()
			status, body, _ := e.post("/api/v1/contact", map[string]any{
				"id": id, "name": "First", "kind": "customer",
			})
			if status != http.StatusCreated {
				t.Fatalf("first create = %d; body=%s", status, body)
			}

			// Another tenant entirely: the row is invisible to this caller, and
			// the refusal must not change that.
			other := seed(t, db)
			status, body, problem := other.post("/api/v1/contact", map[string]any{
				"id": id, "name": "Second", "kind": "customer",
			})
			if status != http.StatusConflict {
				t.Fatalf("duplicate id = %d, want 409; body=%s", status, body)
			}
			detail, _ := problem["detail"].(string)
			if strings.Contains(detail, "First") || strings.Contains(detail, string(e.tenant)) {
				t.Fatalf("the 409 detail names the holder (%q) — a caller-supplied id "+
					"must not become an existence oracle across tenants", detail)
			}
		})
	}
}

// A malformed id is refused rather than replaced. Silently minting one means
// the row the client believes it created is not the row that exists, which is
// the divergence the whole §2 design is there to avoid — arrived at from the
// other side.
func TestCreateRefusesAMalformedID(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			for _, bad := range []any{
				"banana",
				// A v4 UUID: well-formed, wrong version. Ids are UUIDv7 because
				// they sort chronologically (docs/03), and a v4 in the primary
				// key silently costs that.
				"f47ac10b-58cc-4372-a567-0e02b2c3d479",
				// Parseable by uuid.Parse but not the canonical spelling: it
				// would be stored in a different form from the one sent.
				"urn:uuid:018f4b3c-1e2a-7000-8000-000000000001",
				42,
			} {
				status, body, _ := e.post("/api/v1/contact", map[string]any{
					"id": bad, "name": "Malformed", "kind": "customer",
				})
				if status != http.StatusUnprocessableEntity {
					t.Fatalf("create with id %v = %d, want 422; body=%s", bad, status, body)
				}
			}
		})
	}
}

// An absent id still gets one. The offline path is additive: nothing about the
// online UI, which sends no id at all, changes.
func TestCreateStillMintsAnIDWhenNoneIsSupplied(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			if id := createContact(t, e, "Server-numbered"); id == "" {
				t.Fatal("create without an id returned no id")
			}
		})
	}
}

// --- INV-S2: no privileged sync side door ---

// INV-S2 says offline commands pass the identical validation pipeline as
// online writes. WP-2.3-decisions.md §1 makes that structural rather than
// behavioural: a command is a stored HTTP request replayed through the ordinary
// route, so there is no second write path to drift.
//
// This is the test that keeps it structural. A batch replay endpoint under
// /api/v1/sync would be the obvious thing for a later WP to add "for
// efficiency", and it would come with its own authorization, its own error
// mapping and its own divergence from the pipeline it shadows. The invariant
// would then be a promise about two code paths staying in step, which is the
// kind of promise this catalog exists to stop making.
func TestNoSyncWriteEndpointExists(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			// The declared surface: every action registered under /api/v1/sync
			// must be a read.
			for _, a := range allActions(t, db) {
				if !strings.HasPrefix(a.Path, "/api/v1/sync") {
					continue
				}
				if a.Write || a.Method != http.MethodGet {
					t.Errorf("%s %s is a write under /api/v1/sync — the outbox drains through "+
						"the ordinary object routes precisely so there is no second write "+
						"path (INV-S2, WP-2.3-decisions.md §1). If this endpoint is "+
						"deliberate it needs an ADR, not a test edit.", a.Method, a.Path)
				}
			}

			// The served surface. CRUD routes are registered straight onto the
			// mux rather than as Actions (gateway.go registerObject), so an
			// action-table scan alone would miss a hand-wired one. A live probe
			// sees whatever is actually routed.
			e := seed(t, db)
			for _, method := range []string{"POST", "PATCH", "PUT", "DELETE"} {
				for _, path := range []string{
					"/api/v1/sync", "/api/v1/sync/changes", "/api/v1/sync/snapshot",
					"/api/v1/sync/commands", "/api/v1/sync/replay", "/api/v1/sync/outbox",
				} {
					status, body, _ := e.call(method, path, e.token, idgen.New(), map[string]any{})
					if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
						t.Errorf("%s %s answered %d — a sync write endpoint exists and INV-S2 "+
							"is no longer structural; body=%s", method, path, status, body)
					}
				}
			}
		})
	}
}

// The other direction: the routes the drain replays into must be the ordinary
// ones. "No sync endpoint" is satisfied vacuously by there being no write
// surface at all, so the invariant also needs the ordinary object routes to be
// there and to be what an offline command lands on.
func TestDrainReplaysIntoTheOrdinaryObjectRoutes(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			// Exactly the three requests web/src/sync/outbox.ts stores and
			// replays, issued here as the drain issues them: same paths, same
			// method, the command_id as the Idempotency-Key.
			id := idgen.New()
			status, body, _ := e.call("POST", "/api/v1/contact", e.token, idgen.New(),
				map[string]any{"id": id, "name": "Drained", "kind": "customer"})
			if status != http.StatusCreated {
				t.Fatalf("drain-shaped create = %d; body=%s", status, body)
			}
			status, body, _ = e.call("PATCH", "/api/v1/contact/"+id, e.token, idgen.New(),
				map[string]any{"name": "Drained and renamed"})
			if status != http.StatusOK {
				t.Fatalf("drain-shaped update = %d; body=%s", status, body)
			}
			status, body, _ = e.call("DELETE", "/api/v1/contact/"+id, e.token, idgen.New(), nil)
			if status != http.StatusOK && status != http.StatusNoContent {
				t.Fatalf("drain-shaped delete = %d; body=%s", status, body)
			}
		})
	}
}

// A replayed command returns the original response rather than executing
// again. This is the exactly-once effect the drain's retry safety rests on
// (INV-E4): a client that crashes after the server commits and before it
// records the outcome re-sends, and must get the same answer rather than a
// second row.
func TestReplayedCommandIsNotExecutedTwice(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			command := idgen.New()
			body := map[string]any{"id": idgen.New(), "name": "Once", "kind": "customer"}

			status, raw, first := e.call("POST", "/api/v1/contact", e.token, command, body)
			if status != http.StatusCreated {
				t.Fatalf("first send = %d; body=%s", status, raw)
			}
			status, raw, second := e.call("POST", "/api/v1/contact", e.token, command, body)
			if status != http.StatusCreated {
				t.Fatalf("replay = %d, want the stored 201; body=%s", status, raw)
			}
			if mustField(t, first, "id") != mustField(t, second, "id") {
				t.Fatalf("replay produced a different row (%v vs %v) — the drain's retry "+
					"after a crash would duplicate the user's work (INV-E4)", first, second)
			}

			count := 0
			_, _, parsed := e.get("/api/v1/contact")
			rows, _ := parsed["data"].([]any)
			for _, raw := range rows {
				if rec, _ := raw.(map[string]any); rec != nil && rec["name"] == "Once" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%d rows named Once, want 1 — a replayed command executed twice", count)
			}
		})
	}
}
