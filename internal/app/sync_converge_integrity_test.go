//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// WP-2.2b: the client replica converges to server state.
//
// This is the WP's acceptance criterion and it carries **INV-S3** ("client
// replica converges to server state; divergence is detected and repaired,
// never ignored").
//
// Shape, and why it is this shape:
//
//   - The **real server**, over real HTTP, on **both dialects**. The AC said
//     "the real compiled binary"; the harness here boots the wired product
//     handler on an httptest server, which is the same code and additionally
//     runs on Postgres *and* SQLite. A subprocess binary would pin one
//     configuration and prove less.
//   - The **real client core**, unmodified, in Node 26 over `node:sqlite`
//     (web/src/sync/converge-driver.ts). Nothing about the replica is
//     reimplemented in Go — a Go reimplementation would be a second
//     implementation of the thing under test, which is the drift surface
//     ADR-017 rejected Rust to avoid.
//   - The **oracle reads the database directly** through the CRUD engine, not
//     through the sync endpoints. An oracle built from the API under test can
//     only prove the API agrees with itself.
//
// The mutation check below (TestConvergenceHarnessDetectsASkippedFeed) is what
// makes the green tick mean something; read its comment before trusting this
// one.

// convergeRounds is how many randomized operation rounds each dialect runs.
// Each round mutates, syncs, and compares, so the replica is repeatedly caught
// up from a *different* starting cursor rather than hydrated once.
const convergeRounds = 12

// replicaRun drives the TypeScript core against this test's server, reusing one
// replica file across rounds so resume is exercised rather than re-hydration.
type replicaRun struct {
	t      *testing.T
	e      *env
	dbPath string
}

func newReplicaRun(t *testing.T, e *env) *replicaRun {
	t.Helper()
	return &replicaRun{t: t, e: e, dbPath: filepath.Join(t.TempDir(), "replica.sqlite3")}
}

// sync runs one full downstream cycle in the driver and returns the replica's
// tables. dropNth > 0 makes the driver discard every Nth feed entry.
func (r *replicaRun) sync(dropNth int) map[string][]map[string]any {
	r.t.Helper()
	if dropNth > 0 {
		return r.syncWithArgs(fmt.Sprintf("--drop-nth=%d", dropNth))
	}
	return r.syncWithArgs()
}

// syncWithArgs runs the driver under arbitrary fault flags and returns the
// replica's tables. The driver is expected to exit 0: a partition is a state it
// reports, not a failure it propagates (WP-2.3a).
func (r *replicaRun) syncWithArgs(faults ...string) map[string][]map[string]any {
	r.t.Helper()

	out, stderr, err := r.exec(faults...)
	if err != nil {
		// Not a skip. docs/19: "No green gauntlet, no merge — no exceptions."
		// INV-S3 lives in TypeScript because ADR-017 put it there, so a
		// gauntlet that cannot run Node cannot prove it, and saying so loudly
		// is the only honest outcome.
		r.t.Fatalf("replica driver failed: %v\nstderr: %s\n"+
			"this test needs Node 26 on PATH (it strips types natively); "+
			"the integrity-gauntlet CI job installs it", err, stderr)
	}

	var dump map[string][]map[string]any
	if err := json.Unmarshal(out, &dump); err != nil {
		r.t.Fatalf("replica driver emitted unparseable output: %v\n%s", err, out)
	}
	return dump
}

// syncExpectingCrash runs the driver with --kill-in-apply and reports whether
// it actually died there. There is no dump: the process exits mid-transaction,
// which is the whole point.
//
// It returns a bool rather than failing, because "did the crash happen where we
// aimed it" is a property the caller asserts — a driver that completed normally
// means the injection point moved and the crash tests are proving nothing.
func (r *replicaRun) syncExpectingCrash(at int) bool {
	r.t.Helper()
	out := r.syncFaulted(fmt.Sprintf("--kill-in-apply=%d", at))
	if out.err != nil {
		r.t.Fatal(out.err)
	}
	return out.crashed
}

// faultOutcome says which injected faults actually fired. It exists because a
// fault that quietly does not fire is the failure mode of a fault-injection
// harness: the suite still runs, still passes, and no longer tests anything it
// claims to. Every caller that schedules a fault asserts the matching field.
type faultOutcome struct {
	crashed     bool
	partitioned bool
	// stderr is kept so a fault that did not fire can say what the driver did
	// instead. Without it the failure is "the crash did not happen" with no
	// way to tell a missed injection point from a driver that never got there.
	stderr string
	// err is set when the driver failed for a reason that was not an injected
	// fault. It is returned rather than fataled because syncFaulted runs on a
	// spawned goroutine and t.Fatalf is only valid on the test's own — calling
	// it here would Goexit the wrong goroutine and the test would carry on.
	err error
}

// syncFaulted runs the driver under fault flags and reports what fired. The
// dump is discarded: callers that need one sync cleanly afterwards, because a
// mid-fault replica is legitimately partway and not something to compare.
//
// A driver that exits non-zero *without* having emitted a fault marker failed
// for a real reason, and that is reported in the outcome's err. The distinction
// matters more than it looks: an injected crash also exits non-zero with no
// output, so a harness that simply tolerated non-zero exits would read a driver
// that could not even reach the server as a successful fault injection, and
// every scenario built on it would be measuring nothing.
//
// It is safe to call from a goroutine — nothing here touches t. Callers assert
// the outcome on the test's own goroutine (see assertFaults).
func (r *replicaRun) syncFaulted(faults ...string) faultOutcome {
	_, stderr, err := r.exec(faults...)
	out := faultOutcome{
		crashed:     strings.Contains(stderr, driverCrashMarker),
		partitioned: strings.Contains(stderr, driverPartitionMarker),
		stderr:      stderr,
	}
	if err != nil && !out.crashed {
		out.err = fmt.Errorf("replica driver failed for a reason that was not an injected "+
			"fault: %w\nflags: %v\nstderr: %s", err, faults, stderr)
	}
	return out
}

// The markers converge-driver.ts writes to stderr when a fault fires.
//
// The exit *code* is not checked for the crash, because there isn't a portable
// one: exiting from inside a synchronous SQLite call trips a libuv assertion on
// Windows (status 0xc0000409) and exits cleanly with 9 on Linux. A bare
// "non-zero exit" would be portable but far too loose — a driver that failed
// for a real reason also exits non-zero with no dump, and the crash tests would
// then pass on a broken driver. A marker is written only from the injection
// point itself, so it says the fault happened where it was aimed.
const (
	driverCrashMarker     = "lasterp-sync-crash-injected"
	driverPartitionMarker = "lasterp-sync-partition-injected"
)

// exec runs the driver once and returns stdout, stderr, and the run error.
//
// stderr is captured with an explicit buffer rather than read off
// exec.ExitError.Stderr, because that field is only populated when the command
// *fails*. A partition is reported on stderr by a driver that then exits 0, so
// the ExitError path would have made every partition invisible — which is
// exactly how the partition assertion first came to pass on a fault that had
// never fired.
func (r *replicaRun) exec(faults ...string) ([]byte, string, error) {
	r.t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	driver := filepath.Join(filepath.Dir(thisFile), "..", "..", "web", "src", "sync", "converge-driver.ts")

	args := append([]string{driver, r.e.server.URL, r.e.token, r.dbPath}, faults...)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "node", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	return stdout.Bytes(), stderr.String(), err
}

// --- the property ---

func TestReplicaConvergesToProjection(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			// A fixture the generator did not create (WP-2.2b-decisions.md
			// §8.2): rows that exist before the first sync, one with every
			// optional field null, and one archived before the replica has ever
			// seen it. Without these the passing state space is a subset of what
			// the generator happens to produce, and the null branch — which is
			// what bit the audit-pointer defect's cousin — is never sampled.
			seedConvergenceFixture(t, e)

			run := newReplicaRun(t, e)
			rng := rand.New(rand.NewSource(20260803))

			for round := 0; round < convergeRounds; round++ {
				randomOps(t, e, rng)
				assertConverged(t, e, run.sync(0), fmt.Sprintf("round %d", round))
			}
		})
	}
}

// The green tick above is only worth what this test says it is.
//
// Materialised rows are *current* server state (WP-2.2-decisions.md §2) and the
// oracle is current server state, so a convergence harness that syncs to
// quiescence and then compares can pass **by construction**: the last pass
// re-fetches every touched row and overwrites whatever an earlier bug damaged.
// Such a test measures that SELECT equals SELECT. It would not have caught
// WP-2.1's audit-pointer defect either — that was green in CI and found by a
// human building the first consumer (WP-2.2-decisions.md §1a).
//
// So: delete entries from the feed on the way to the client and require the
// property to FAIL. If it still passes, TestReplicaConvergesToProjection is not
// testing selection and its result must not be trusted.
func TestConvergenceHarnessDetectsASkippedFeed(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)

			// Hydrate first, so the divergence can only come from the feed:
			// after this the replica is correct and every later row arrives as
			// a change entry.
			assertConverged(t, e, run.sync(0), "before mutation")

			for i := 0; i < 24; i++ {
				createContact(t, e, fmt.Sprintf("Skipped %d", i))
			}

			// Every 2nd entry and its row never reach the core.
			if converged, _ := convergence(t, e, run.sync(2)); converged {
				t.Fatal("the replica still matched the server with half the feed deleted — " +
					"the convergence property is tautological and INV-S3 is not proven by it")
			}
		})
	}
}

// A replica must never receive another tenant's row (INV-T1). The replica
// carries tenant_id precisely so this is falsifiable on the client rather than
// only asserted on the server.
func TestReplicaNeverReceivesAnotherTenantsRows(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			createContact(t, e, "Mine")

			other := seed(t, db)
			createContact(t, other, "Theirs")

			dump := newReplicaRun(t, e).sync(0)
			for object, rows := range dump {
				for _, row := range rows {
					if got := fmt.Sprint(row["tenant_id"]); got != string(e.tenant) {
						t.Fatalf("%s row %v carries tenant %s, want %s — a replica holding "+
							"another tenant's row is INV-T1 defeated at the last hop",
							object, row["id"], got, e.tenant)
					}
				}
			}
		})
	}
}

// --- operations ---

// randomOps performs a handful of creates, updates and soft-deletes across both
// generic CRUD objects, so a round exercises every change kind the feed carries.
func randomOps(t *testing.T, e *env, rng *rand.Rand) {
	t.Helper()
	for i := 0; i < 3+rng.Intn(4); i++ {
		switch rng.Intn(4) {
		case 0:
			createContact(t, e, fmt.Sprintf("Gen %d", rng.Int63()))
		case 1:
			e.createAccount(fmt.Sprintf("A%d", rng.Int63()), fmt.Sprintf("Acct %d", rng.Intn(999)),
				[]string{"asset", "liability", "equity", "income", "expense"}[rng.Intn(5)])
		case 2:
			if id := anyLiveID(t, e, "contact"); id != "" {
				status, body, _ := e.call("PATCH", "/api/v1/contact/"+id, e.token, idgen.New(),
					map[string]any{"name": fmt.Sprintf("Renamed %d", rng.Intn(9999))})
				if status != http.StatusOK {
					t.Fatalf("patch contact = %d; body=%s", status, body)
				}
			}
		case 3:
			if id := anyLiveID(t, e, "contact"); id != "" {
				status, body, _ := e.call("DELETE", "/api/v1/contact/"+id, e.token, idgen.New(), nil)
				if status != http.StatusOK && status != http.StatusNoContent {
					t.Fatalf("delete contact = %d; body=%s", status, body)
				}
			}
		}
	}
}

// anyLiveID returns some non-archived row's id, or "" when there are none.
func anyLiveID(t *testing.T, e *env, resource string) string {
	t.Helper()
	status, body, parsed := e.get("/api/v1/" + resource)
	if status != http.StatusOK {
		t.Fatalf("list %s = %d; body=%s", resource, status, body)
	}
	rows, _ := parsed["data"].([]any)
	if len(rows) == 0 {
		return ""
	}
	rec, _ := rows[len(rows)-1].(map[string]any)
	id, _ := rec["id"].(string)
	return id
}

// seedConvergenceFixture plants rows the generator did not create: one with
// every optional field left null, and one archived before the replica exists.
func seedConvergenceFixture(t *testing.T, e *env) {
	t.Helper()

	status, body, _ := e.post("/api/v1/contact", map[string]any{
		"name": "Sparse Fixture", "kind": "customer",
		// email and locale deliberately absent — the null branch.
	})
	if status != http.StatusCreated {
		t.Fatalf("seed sparse contact = %d; body=%s", status, body)
	}

	gone := createContact(t, e, "Archived Before First Sync")
	status, body, _ = e.call("DELETE", "/api/v1/contact/"+gone, e.token, idgen.New(), nil)
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("seed archived contact = %d; body=%s", status, body)
	}
}

// --- the oracle ---

// assertConverged fails the test when the replica and the server disagree.
func assertConverged(t *testing.T, e *env, dump map[string][]map[string]any, when string) {
	t.Helper()
	if ok, detail := convergence(t, e, dump); !ok {
		t.Fatalf("replica diverged from the server projection at %s:\n%s", when, detail)
	}
}

// convergence compares the replica's tables to the server's projection, read
// straight from the database through the CRUD engine rather than through the
// sync endpoints — an oracle built from the API under test can only prove the
// API agrees with itself.
func convergence(t *testing.T, e *env, dump map[string][]map[string]any) (bool, string) {
	t.Helper()
	ctx := e.actorCtx(t, fullGrants())

	schemas, err := crudObjects()
	if err != nil {
		t.Fatalf("crudObjects: %v", err)
	}

	var problems []string
	for _, schema := range schemas {
		crud, err := metadata.NewCRUD(schema)
		if err != nil {
			t.Fatalf("NewCRUD %s: %v", schema.ObjectName, err)
		}
		records, err := crud.List(ctx, e.db, e.tenant)
		if err != nil {
			t.Fatalf("list %s: %v", schema.ObjectName, err)
		}

		want := canonical(schema, recordsToMaps(records))
		got := canonical(schema, dump[schema.ObjectName])

		for id, wantRow := range want {
			gotRow, ok := got[id]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"%s %s: on the server, absent from the replica", schema.ObjectName, id))
				continue
			}
			if wantRow != gotRow {
				problems = append(problems, fmt.Sprintf(
					"%s %s:\n  server:  %s\n  replica: %s", schema.ObjectName, id, wantRow, gotRow))
			}
		}
		for id := range got {
			if _, ok := want[id]; !ok {
				problems = append(problems, fmt.Sprintf(
					"%s %s: in the replica, not on the server (a delete that was never applied)",
					schema.ObjectName, id))
			}
		}
	}

	sort.Strings(problems)
	return len(problems) == 0, strings.Join(problems, "\n")
}

func recordsToMaps(records []metadata.Record) []map[string]any {
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		out = append(out, map[string]any(r))
	}
	return out
}

// canonical renders each row as id -> a stable string over the declared fields,
// so the comparison is dialect- and driver-independent. Booleans, integers and
// timestamps all arrive differently on the two sides (SQLite stores 0/1; Go
// hands back time.Time; JSON hands back a string), and normalising is what lets
// the assertion be exact equality rather than a fuzzy match.
func canonical(schema *metadata.EffectiveSchema, rows []map[string]any) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		id := fmt.Sprint(row["id"])

		parts := []string{"updated_at=" + normalize(row["updated_at"])}
		for _, f := range schema.Fields {
			parts = append(parts, f.Name+"="+normalize(row[f.Name]))
		}
		out[id] = strings.Join(parts, " ")
	}
	return out
}

// normalize renders one value the same way whichever side produced it.
func normalize(v any) string {
	switch t := v.(type) {
	case nil:
		return "∅"
	case time.Time:
		// Postgres keeps microseconds, SQLite's round-trip can differ in the
		// tail; truncating is honest about the precision both actually carry.
		return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		// JSON's only numeric type. Integer columns come back as float64 from
		// the driver's JSON, so render without an exponent or trailing zeros.
		return fmt.Sprintf("%v", int64(t))
	case string:
		// A timestamp that crossed JSON is a string on the replica side and a
		// time.Time on the server's; parse it back so the two compare equal.
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
		}
		return t
	default:
		return fmt.Sprint(t)
	}
}
