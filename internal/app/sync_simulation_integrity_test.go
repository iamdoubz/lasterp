//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/changefeed"
)

// WP-2.3a: the deterministic simulation harness docs/04 §Test plan asks for —
// "N virtual clients × scripted partitions/crashes/interleavings × property
// checks".
//
// It is a growth of TestReplicaConvergesToProjection rather than a second
// harness. That test proved one client catches up when nothing goes wrong; this
// one keeps the same server, the same unmodified TypeScript core and the same
// direct-from-the-database oracle, and adds the weather:
//
//   - **N clients**, each with its own replica file and its own driver process,
//     synced concurrently against one server.
//   - **Partitions** — the wire is cut mid-sync (`--fail-after`), so the client
//     is left partway and must converge once it is back.
//   - **Crashes** — the process dies *inside* an apply transaction
//     (`--kill-in-apply`), after rows are written and before the cursor moves.
//     This is the one fault that cannot be injected through the Transport, and
//     it is the only thing that turns core.ts's crash-safety comment into a
//     claim a test can refute.
//   - **Interleavings** — clients are stopped at different points and resume
//     from different cursors, so no two are reading the same part of the feed.
//
// The properties it carries are the downstream ones: **INV-S3** (the replica
// converges) and **INV-S5** (no committed change is skipped; a stable total
// order that does not change on resume). The upstream properties — INV-S1/S2/S4
// — need an outbox and land with WP-2.3b, which adds scenarios to *this* file
// rather than building a third harness. See WP-2.3-decisions.md §0.
//
// Read TestSimulationHarnessDetectsADivergentClient before trusting any green
// tick from here, for the reason spelled out in
// TestConvergenceHarnessDetectsASkippedFeed.

const (
	// simClients is N. Four is enough for concurrent readers at distinct
	// cursors without making the suite a subprocess farm: each client is a
	// node process per round, per dialect.
	simClients = 4
	// simRounds is how many mutate-then-sync cycles the schedule runs. Each
	// round re-partitions and re-crashes a different client, so over the run
	// every client has been interrupted at least once.
	simRounds = 6
	// simSeed makes the schedule deterministic: a failure reproduces.
	simSeed = 20260804
)

// fault is one client's round: what its user did, and what went wrong. The
// zero value is a clean sync with no offline work, which is deliberate — most
// rounds for most clients should be ordinary, or the harness only ever tests
// the exceptional path.
type fault struct {
	failAfter   int // >0: partition after this many transport calls
	killInApply int // >0: exit inside the apply transaction, at this row write
	// queue is work done offline before this round's sync (WP-2.3b). It is on
	// this struct rather than beside it because the interesting scenarios are
	// the combinations: queued work *and* a partition is a client that wrote
	// something and then could not send it, which is the state INV-S1 is a
	// promise about.
	queue int
	label string
}

func (f fault) args() []string {
	var out []string
	if f.failAfter > 0 {
		out = append(out, fmt.Sprintf("--fail-after=%d", f.failAfter))
	}
	if f.killInApply > 0 {
		out = append(out, fmt.Sprintf("--kill-in-apply=%d", f.killInApply))
	}
	if f.queue > 0 {
		out = append(out, fmt.Sprintf("--enqueue=%d", f.queue), "--label="+f.label)
	}
	return out
}

// --- the harness ---

// simClient is one virtual client: a replica file that persists across rounds,
// so resume is exercised rather than re-hydration.
type simClient struct {
	name string
	run  *replicaRun
}

func newSimClients(t *testing.T, e *env, n int) []*simClient {
	t.Helper()
	out := make([]*simClient, n)
	for i := range out {
		out[i] = &simClient{name: fmt.Sprintf("client-%d", i), run: newReplicaRun(t, e)}
	}
	return out
}

// --- the properties ---

// TestSimulationConvergesUnderFaults is the harness proper: N clients, a
// scripted schedule of partitions and crashes over concurrent server mutation,
// and the requirement that every client converges to the server's projection
// once the faults stop.
//
// Convergence is only asserted after a final fault-free sync, and that is not a
// weakening. It is a statement about where a replica *settles*, not about where
// it is mid-flight; a partitioned client is legitimately behind, and requiring
// otherwise would be requiring that partitions do not exist. What stops that
// from making the whole thing vacuous is TestSimulationHarnessDetectsADivergent-
// Client, which proves the final comparison can still fail.
func TestSimulationConvergesUnderFaults(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			seedConvergenceFixture(t, e)

			clients := newSimClients(t, e, simClients)
			rng := rand.New(rand.NewSource(simSeed))

			// Hydrate everyone cleanly first, so a later divergence can only
			// have come from the feed rather than from a half-built replica.
			syncAll(t, clients, make([]fault, len(clients)))

			for round := 0; round < simRounds; round++ {
				randomOps(t, e, rng)
				// A floor of live rows for the crash injection to land inside,
				// created *after* the random ops and never before: randomOps'
				// delete case picks the newest contact, so fixtures created
				// first get archived and arrive as DELETEs rather than the row
				// writes the crash counts. See roundFaults.
				for i := 0; i < crashFloor; i++ {
					createContact(t, e, fmt.Sprintf("Round %d fixture %d", round, i))
				}
				syncAll(t, clients, roundFaults(len(clients), round))
			}

			// The wire is back and nothing crashes: everyone must land on the
			// same state as the server, and everything their users wrote
			// offline must have got there.
			//
			// Two clean syncs each, not one: a client whose last round was cut
			// mid-drain legitimately still holds commands, and stop-on-reject
			// means a queue can need more than one pass. What is *not*
			// legitimate is work that never settles, which is what the
			// assertions below are for.
			settled := make([]map[string][]map[string]any, len(clients))
			for i, c := range clients {
				c.run.sync(0)
				settled[i] = c.run.sync(0)
				assertConverged(t, e, settled[i], c.name+" after the storm")
			}
			assertNoWorkWasLost(t, e, clients, settled)
		})
	}
}

// assertNoWorkWasLost is INV-S1 and INV-S4 over the whole run: every command
// any client queued is now on the server, nothing is still stuck in an outbox,
// and no fault was mistaken for a rejection.
//
// The count is exact rather than a floor. "At least as many as we queued" would
// pass on a client that sent one command twice, and that is the other half of
// RPO 0 — a write that survives a crash is no good if it survives it twice
// (INV-E4).
// settled is each client's dump from its own final clean sync — read from
// there rather than re-running the driver, which would be four more subprocess
// spawns per dialect for state already in hand. internal/app is the gauntlet's
// long pole and every avoidable driver run is paid on every CI run.
func assertNoWorkWasLost(t *testing.T, e *env, clients []*simClient, settled []map[string][]map[string]any) {
	t.Helper()

	wanted := len(clients) * simRounds * offlineWritesPerRound
	if got := serverCountNamed(t, e, clientWrittenPrefix); got != wanted {
		t.Fatalf("%d offline writes reached the server, want exactly %d — the fleet %s work "+
			"across partitions and crashes (INV-S1/INV-E4)", got, wanted,
			map[bool]string{true: "duplicated", false: "lost"}[got > wanted])
	}

	for i, c := range clients {
		dump := settled[i]
		if queued := len(dump["_outbox"]); queued != 0 {
			t.Fatalf("%s still has %d commands queued after the storm — they are neither "+
				"accepted nor surfaced, which is the silent stall INV-S4 covers: %v",
				c.name, queued, dump["_outbox"])
		}
		if filed := len(dump["_conflicts"]); filed != 0 {
			t.Fatalf("%s filed %d conflicts for work the server had no reason to refuse: %v — "+
				"a fault was mistaken for a rejection", c.name, filed, dump["_conflicts"])
		}
	}
}

// crashFloor is how many live rows each round is guaranteed to add, so that a
// crash aimed at row write 1..crashFloor always has somewhere to land.
//
// Without it the injection silently misses: randomOps produces three to six
// operations of which only some are inserts, so a crash aimed at the fourth row
// write of a caught-up client frequently finds only two and the driver exits
// normally. That is how the first version of this harness ran six rounds on
// both dialects injecting ten crashes, firing none of them, and reporting green
// — which is why every scheduled fault is now asserted rather than assumed.
const crashFloor = 3

// offlineWritesPerRound is how much work each client's user does between
// syncs. Every client, every round: the upstream path should be the ordinary
// one, not a special case the schedule visits occasionally.
const offlineWritesPerRound = 2

// roundFaults is the schedule: every client queues offline work, one is
// partitioned and a different one is crashed each round, rotating, so every
// client is interrupted over the run and no client is interrupted twice
// running.
//
// failAfter is 1, not a random value, and that is deliberate. Transport calls
// are counted from the start of the sync, and a caught-up client makes exactly
// two (metadata, then one feed page), so any threshold above 1 lets the sync
// finish and the "partition" never happens. Deterministic beats varied when the
// alternative is a fault that only sometimes exists.
//
// With WP-2.3b that threshold also lands somewhere new: the drain runs before
// the pull (WP-2.3-decisions.md §4), so a partition after one call now cuts the
// wire with commands still queued. That is the point — the partitioned client
// of each round is one that wrote something offline and could not send it, and
// the final assertion is that it eventually does.
func roundFaults(n, round int) []fault {
	faults := make([]fault, n)
	for i := range faults {
		faults[i] = fault{
			queue: offlineWritesPerRound,
			// clientWrittenPrefix: randomOps must not archive these, or the
			// count of delivered work would measure the churn instead.
			label: fmt.Sprintf("%sc%d r%d", clientWrittenPrefix, i, round),
		}
	}
	faults[round%n].failAfter = 1
	faults[(round+1)%n].killInApply = 1 + round%crashFloor
	return faults
}

// syncAll runs every client's round concurrently and requires every scheduled
// fault to have actually fired.
//
// Concurrency is the point rather than a speed-up: clients reading the feed at
// overlapping times from distinct cursors is the interleaving docs/04 asks for,
// and it is also what puts the per-tenant ordering lock (WP-2.1-decisions.md
// §2) under contention.
func syncAll(t *testing.T, clients []*simClient, faults []fault) {
	t.Helper()
	outcomes := make([]faultOutcome, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i] = c.run.syncFaulted(faults[i].args()...)
		}()
	}
	wg.Wait()

	// Assertions run here, on the test's own goroutine: t.Fatalf from a spawned
	// one Goexits the wrong goroutine and the test carries on regardless.
	for i, f := range faults {
		if err := outcomes[i].err; err != nil {
			t.Fatalf("%s: %v", clients[i].name, err)
		}
		if f.killInApply > 0 && !outcomes[i].crashed {
			t.Fatalf("%s was scheduled to crash at row write %d and did not — the fault "+
				"injection is not reaching the apply transaction, so this harness is not "+
				"testing crashes\ndriver stderr: %s",
				clients[i].name, f.killInApply, outcomes[i].stderr)
		}
		if f.failAfter > 0 && !outcomes[i].partitioned {
			t.Fatalf("%s was scheduled to partition after %d transport calls and did not — "+
				"the sync completed, so this harness is not testing partitions\ndriver stderr: %s",
				clients[i].name, f.failAfter, outcomes[i].stderr)
		}
	}
}

// TestCrashMidApplyLosesNoFeedEntry isolates the crash case.
//
// core.ts says rows and cursor "move in one transaction. If this throws
// mid-page, the replica is exactly where it started". A thrown exception proves
// the rollback path; it does not prove the *crash* path, because an exception
// unwinds cleanly and a killed tab does not. So: kill the process with the
// transaction open and rows already written, then reopen and require both that
// the replica converges and that the cursor never claimed the rows it lost.
func TestCrashMidApplyLosesNoFeedEntry(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before the crash")

			for i := 0; i < 16; i++ {
				createContact(t, e, fmt.Sprintf("Crash %d", i))
			}

			if !run.syncExpectingCrash(3) {
				t.Fatal("the driver was asked to exit inside the apply transaction and did not — " +
					"the crash path is not being exercised and this test proves nothing")
			}

			// Reopening rolls the hot journal back. The replica must be intact
			// and must not have skipped the entries it was mid-way through.
			assertConverged(t, e, run.sync(0), "after reopening a crashed replica")
		})
	}
}

// TestConcurrentClientsObserveTheSameFeedOrder is INV-S5 under contention:
// "for any cursor position a reader observes every committed entry exactly
// once, in a stable total order that does not change on resume."
//
// The existing feed tests read sequentially from one reader. This reads the
// same feed from N readers at staggered cursors while writes are landing, and
// requires that the sequences they observe are prefixes of one another — which
// is what "one stable total order" means operationally. A feed that renumbered
// or reordered under concurrent readers would show up as two clients holding
// incompatible sequences for the same cursor range.
func TestConcurrentClientsObserveTheSameFeedOrder(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)

			// Writes land while the readers are running, so the readers see a
			// moving feed rather than a frozen one.
			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := 0; i < 20; i++ {
					createContact(t, e, fmt.Sprintf("Concurrent %d", i))
				}
			}()

			observed := make([][]int64, simClients)
			var wg sync.WaitGroup
			for i := range observed {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Staggered starting cursors: each reader begins where the
					// one before it would be a few entries in.
					observed[i] = readFeedCursors(t, e, int64(i))
				}()
			}
			wg.Wait()
			<-done

			for i, seq := range observed {
				if !ascending(seq) {
					t.Fatalf("reader %d observed a non-ascending feed: %v — INV-S5 requires a "+
						"stable total order", i, seq)
				}
				if dup := firstDuplicate(seq); dup != 0 {
					t.Fatalf("reader %d observed cursor %d twice: %v — INV-S5 requires "+
						"exactly once", i, dup, seq)
				}
			}
			assertNoReaderSkippedAnEntry(t, observed)
		})
	}
}

// readFeedCursors drains the feed from `after` and returns the cursors it saw.
func readFeedCursors(t *testing.T, e *env, after int64) []int64 {
	t.Helper()
	var seen []int64
	for {
		status, body, parsed := e.get(fmt.Sprintf("/api/v1/sync/changes?after=%d&limit=50", after))
		if status != http.StatusOK {
			t.Errorf("read feed after %d = %d; body=%s", after, status, body)
			return seen
		}
		entries, _ := parsed["data"].([]any)
		if len(entries) == 0 {
			return seen
		}
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			cursor, _ := entry["cursor"].(float64)
			seen = append(seen, int64(cursor))
			after = int64(cursor)
		}
	}
}

// assertNoReaderSkippedAnEntry is the cross-reader half of INV-S5: "no
// committed change is skipped ... for any cursor position a reader observes
// every committed entry exactly once".
//
// Readers start at staggered cursors and stop when they catch up, so their
// sequences are legitimately different — one may begin later, another may end
// earlier. What is *not* legitimate is a hole: if any reader saw cursor c, then
// every reader whose own range spans c must have seen it too. A reader that
// jumped from 41 to 43 while its neighbour saw 42 skipped a committed entry,
// and that is the failure INV-S5 names.
//
// This is deliberately not a check that each sequence is sorted — ascending()
// already covers that, and re-asserting it here would look like a cross-reader
// property while comparing each reader only against itself.
func assertNoReaderSkippedAnEntry(t *testing.T, observed [][]int64) {
	t.Helper()

	union := map[int64]bool{}
	for _, seq := range observed {
		for _, c := range seq {
			union[c] = true
		}
	}
	all := make([]int64, 0, len(union))
	for c := range union {
		all = append(all, c)
	}
	sort.Slice(all, func(a, b int) bool { return all[a] < all[b] })

	for i, seq := range observed {
		if len(seq) == 0 {
			continue // read nothing; no range to be inconsistent over
		}
		seen := map[int64]bool{}
		for _, c := range seq {
			seen[c] = true
		}
		lo, hi := seq[0], seq[len(seq)-1]
		for _, c := range all {
			if c < lo || c > hi || seen[c] {
				continue
			}
			t.Fatalf("reader %d skipped cursor %d, which another reader observed inside "+
				"reader %d's own range [%d, %d]: %v — INV-S5 requires every committed "+
				"entry to be observed", i, c, i, lo, hi, seq)
		}
	}
}

func ascending(seq []int64) bool {
	for i := 1; i < len(seq); i++ {
		if seq[i] <= seq[i-1] {
			return false
		}
	}
	return true
}

func firstDuplicate(seq []int64) int64 {
	seen := map[int64]bool{}
	for _, v := range seq {
		if seen[v] {
			return v
		}
		seen[v] = true
	}
	return 0
}

// The harness's green tick is worth exactly what this test says it is.
//
// Same argument as TestConvergenceHarnessDetectsASkippedFeed one file over, and
// it has to be restated here because the harness is a *different* comparison:
// N clients settling on server state after faults could pass by construction if
// the final clean sync simply re-fetches everything. So: run one client of the
// N against a feed with entries deleted, and require the harness to notice.
//
// If a fleet of clients can be half-blinded and the suite still goes green, the
// suite is measuring that SELECT equals SELECT and INV-S3 is not proven by it.
func TestSimulationHarnessDetectsADivergentClient(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			clients := newSimClients(t, e, simClients)
			syncAll(t, clients, make([]fault, len(clients)))

			for i := 0; i < 24; i++ {
				createContact(t, e, fmt.Sprintf("Unseen %d", i))
			}

			// Every client but one syncs cleanly and must converge; the odd one
			// out loses half its feed and must not.
			for _, c := range clients[1:] {
				assertConverged(t, e, c.run.sync(0), c.name+" (clean)")
			}
			if converged, _ := convergence(t, e, clients[0].run.sync(2)); converged {
				t.Fatal("a client that never received half its feed still matched the server — " +
					"the harness cannot detect divergence and its INV-S3 result must not be trusted")
			}
		})
	}
}

// A crashed client must not be able to hide a divergence either: the recovery
// path is what the crash test asserts, so the crash itself has to be real.
// This proves the injection point does what it says — the process dies with
// rows written and the cursor not yet advanced, so the replica on disk is
// *behind* the entries it had begun applying.
func TestCrashLeavesTheCursorBehindTheRowsItWasApplying(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			run := newReplicaRun(t, e)
			assertConverged(t, e, run.sync(0), "before the crash")

			before := feedHighWater(t, e)
			for i := 0; i < 12; i++ {
				createContact(t, e, fmt.Sprintf("Rolled back %d", i))
			}
			if after := feedHighWater(t, e); after <= before {
				t.Fatalf("feed high-water did not advance (%d -> %d); the crash window "+
					"cannot exist", before, after)
			}

			if !run.syncExpectingCrash(2) {
				t.Fatal("the driver did not crash where it was told to")
			}

			// The crashed replica is behind: the rows it was mid-way through
			// were rolled back with the transaction. Nothing was half-committed.
			if converged, _ := convergence(t, e, run.sync(0)); !converged {
				t.Fatal("a crashed replica did not repair itself on the next sync")
			}
		})
	}
}

func feedHighWater(t *testing.T, e *env) int64 {
	t.Helper()
	high, err := changefeed.HighWater(e.actorCtx(t, fullGrants()), e.db, e.tenant)
	if err != nil {
		t.Fatalf("changefeed.HighWater: %v", err)
	}
	return high
}
