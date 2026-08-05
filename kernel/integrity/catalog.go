// SPDX-License-Identifier: AGPL-3.0-only

// Package integrity is the WP-0.8 foundation for docs/19: the invariant
// catalog as code, the DB role-separation helper that makes append-only
// tables un-mutable rather than merely trigger-guarded, and (in tests) the
// adversarial writer suite that proves every known bypass fails.
//
// This package is invariant-enforcement code in the sense of docs/19 §3:
// changes here belong under CODEOWNERS review and are outside every
// autonomous path (INV-X4). It deliberately imports only kernel/storage so
// it stays a leaf the whole kernel can depend on.
package integrity

// Layer names the docs/19 §2 enforcement layer an invariant is anchored in.
// It is descriptive metadata for the catalog, not behaviour.
type Layer string

const (
	LayerType     Layer = "type-system" // money types, tenant-scoped repos, codegen
	LayerStorage  Layer = "storage"     // DB constraints, RLS, append-only grants/triggers
	LayerPipeline Layer = "command"     // single write choke point + role separation
	LayerSentinel Layer = "runtime"     // continuous verification (docs/13)
)

// Invariant is one row of the docs/19 catalog, transcribed as code so the
// catalog has a single machine-checked source of truth.
type Invariant struct {
	// ID is the catalog identifier, e.g. "INV-E1". It is the tag tests
	// reference (in a comment) to claim coverage.
	ID string
	// Title is the one-line statement from docs/19.
	Title string
	// Layer is the primary enforcement layer.
	Layer Layer
	// TestRequired is true once the enforcing code exists and CI must find a
	// tagged test for this ID (TestEveryRequiredInvariantHasATaggedTest). It
	// is false for invariants whose module has not been built yet; that
	// module's WP flips it to true when it lands its enforcement + tests.
	TestRequired bool
	// AppendOnlyTables lists tables this invariant makes immutable. Non-empty
	// only for the append-only invariants (INV-E1, INV-T4); EnforceAppendOnly-
	// Grants revokes UPDATE/DELETE/TRUNCATE on exactly these.
	AppendOnlyTables []string
	// Note records why TestRequired is false, e.g. the WP that will enable it.
	Note string
}

// Catalog is the full docs/19 invariant catalog. INV-E and INV-T are
// enforced and tested as of WP-0.8 (Phase 0 write surface); INV-F/S/X are
// registered but their enforcement lands with later phases — each module WP
// flips TestRequired and adds tagged tests as part of its own acceptance
// criteria (docs/19: "New modules MUST register their invariants here").
var Catalog = []Invariant{
	// Financial (INV-F) — Phase 1 (ledger → money → tax → inventory).
	{ID: "INV-F1", Title: "Every journal entry balances (Σdebits = Σcredits per currency, to the minor unit)", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-F2", Title: "Posted financial documents are immutable; corrections are reversing events only", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-F3", Title: "No posting into a closed period; period close is monotonic", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-F4", Title: "Money is integer minor units + ISO-4217; no floats; allocation conserves every cent", Layer: LayerType, TestRequired: true},
	{ID: "INV-F5", Title: "Financially-relevant documents post to GL only through their declared template", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-F6", Title: "Document number sequences are gapless-per-policy, assigned only at server acceptance", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-F7", Title: "Stock quantity × valuation reconciles with GL inventory accounts (bounded lag)", Layer: LayerSentinel, Note: "lands with WP-4.4 inventory"},
	{ID: "INV-F8", Title: "Settlement never exceeds the document it settles: Σapplied ≤ gross, no negative application", Layer: LayerPipeline, TestRequired: true},

	// Event store (INV-E) — enforced as of WP-0.4/0.8.
	{ID: "INV-E1", Title: "Streams are append-only; no UPDATE/DELETE on the events table", Layer: LayerStorage, TestRequired: true, AppendOnlyTables: []string{"events"}},
	{ID: "INV-E2", Title: "Optimistic concurrency: version conflicts are rejected, never silently merged", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-E3", Title: "Events are immutable post-commit; schema evolution via upcasters only", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-E4", Title: "command_id is unique: replay/retry produces exactly-once effects", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-E5", Title: "Projections are pure functions of the log: rebuild(events) ≡ projection", Layer: LayerSentinel, TestRequired: true},

	// Tenancy & access (INV-T) — enforced as of WP-0.3/0.5/0.6/0.8.
	{ID: "INV-T1", Title: "No query path returns another tenant's rows (RLS backstop; zero rows without context)", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-T2", Title: "No write path executes without an authenticated principal and authz decision", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-T3", Title: "Permission floors, approval gates and declared value domains cannot be widened by overlays/plugins/agents", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-T4", Title: "Every mutation is attributable: actor, command, timestamp — no anonymous writes", Layer: LayerStorage, TestRequired: true, AppendOnlyTables: []string{"audit_log"}},
	{ID: "INV-T5", Title: "Every stored field value conforms to its object's effective schema (declared type and option set)", Layer: LayerPipeline, TestRequired: true},

	// Sync (INV-S) — Phase 2.
	// INV-S1 flips in WP-2.3b, where there is an outbox to lose a write from.
	// The proof is one crash window: the process dies after the server has
	// committed and before the client's record of having sent it is durable
	// (TestCrashBetweenAcceptanceAndAcknowledgementLosesNothing). Recovery must
	// re-send and be deduplicated — the count on the server is asserted exactly,
	// because a write that survives twice fails RPO 0 as surely as one that does
	// not survive at all.
	{ID: "INV-S1", Title: "No acknowledged write is ever lost (RPO 0)", Layer: LayerPipeline, TestRequired: true},
	// INV-S2 is structural rather than behavioural: a command is a stored HTTP
	// request replayed through the ordinary route, so there is no second write
	// path to drift from the first (WP-2.3-decisions.md §1). The tests assert
	// that no write endpoint exists under /api/v1/sync — in the action table and
	// on the live mux — and that a drained command is refused with the identical
	// status and title as the same body sent online.
	{ID: "INV-S2", Title: "Offline commands pass the identical validation pipeline as online writes", Layer: LayerPipeline, TestRequired: true},
	// INV-S3 flips here rather than in WP-2.2a because convergence is a
	// property of a replica, and PR-A had no replica to converge. It is proven
	// by TestReplicaConvergesToProjection (randomized operations, real server,
	// both dialects, the real TypeScript core over node:sqlite) — and that
	// proof is only worth what TestConvergenceHarnessDetectsASkippedFeed says
	// it is: delete entries from the feed and the property must fail, or it was
	// measuring that SELECT equals SELECT.
	{ID: "INV-S3", Title: "Client replica converges to server state; divergence is detected and repaired", Layer: LayerSentinel, TestRequired: true},
	// INV-S4 is conservation: every command that enters the outbox leaves it
	// accepted or filed where a person can see it, and the counts add up
	// (TestEveryCommandReachesExactlyOneTerminalState). "Surfaced" also covers
	// the silent *stall* — a command retrying forever at the head of an ordered
	// queue blocks everything behind it, so retries are capped and the survivor
	// is filed like any other rejection.
	//
	// **One sanctioned exception, added by WP-2.5: a remote wipe.** A wipe
	// destroys queued commands that reached no terminal state. The
	// reconciliation is that INV-S4 is a promise to *the user of a device*, and
	// a wipe is an administrator's statement that this device has no legitimate
	// user — there is nobody on it to surface anything to. Nothing else may
	// claim this exemption without amending this note (WP-2.5-decisions.md §5).
	{ID: "INV-S4", Title: "Rejected commands are surfaced to the user; no silent drops", Layer: LayerPipeline, TestRequired: true},
	// INV-S5 is the downstream half of INV-S1, and lands early because the
	// feed exists before the replica does (WP-2.1). It is separate rather than
	// folded in: INV-S1 is the end-to-end RPO-0 promise that only completes
	// once upstream command replay exists in WP-2.3, and claiming it here
	// would overstate what is proven.
	{ID: "INV-S5", Title: "No committed change is skipped by the feed: every entry is observed exactly once, in a stable total order", Layer: LayerPipeline, TestRequired: true, AppendOnlyTables: []string{"change_feed"}},

	// Device (INV-D) — Phase 2, WP-2.5.
	//
	// INV-D1 is why the wipe check lives in the authenticator rather than at
	// any endpoint: a control the subject can decline to receive is not one.
	// The test enumerates the live mux and asserts no authenticated route
	// serves a wiped device, in the same shape as WP-2.3b's
	// TestNoSyncWriteEndpointExists — a property of the whole surface, not of
	// the handlers somebody remembered to check.
	//
	// Note what it deliberately does NOT claim: that the device deleted
	// anything. The server can prove refusal and delivery; it cannot prove
	// erasure on a disk it does not own (WP-2.5-decisions.md §4). At-rest
	// protection of the replica itself is ADR-021 and WP-4.8.
	{ID: "INV-D1", Title: "A device marked wiped is refused on every authenticated path", Layer: LayerPipeline, TestRequired: true},

	// Secret material (INV-K) — Phase 3, WP-3.0.
	//
	// Nothing already in this catalog covered "this value must never be stored
	// in the clear": INV-T1 is cross-tenant reads and INV-T4 is attribution.
	// The vault is invariant-bearing code, so it registers its own
	// (WP-3.0-decisions.md §7). K is for key material; P is left free for
	// docs/20's privacy invariants.
	//
	// The load-bearing half is structural rather than behavioural, in the shape
	// of WP-2.3b's TestNoSyncWriteEndpointExists: no route returns secret
	// material, asserted against the live mux and the action table, so adding a
	// reveal endpoint fails CI rather than review. The rest is measured where
	// plaintext could plausibly surface — the row itself, the audit log, the
	// event store, the change feed, a replica hydration snapshot — with the
	// known plaintext searched for in both raw and base64 form, since the
	// columns are base64 (§10).
	{ID: "INV-K1", Title: "Secret material is never persisted, logged, emitted or replicated in plaintext", Layer: LayerStorage, TestRequired: true},

	// Extension & autonomy (INV-X) — Phase 3 / Phase 6.
	// INV-X1 flips in WP-3.1a. Its enforcement is stronger than "the host
	// function checks a capability": the host-function *table* is built from
	// the approved capabilities, so a module importing something it was not
	// granted cannot be instantiated — it is refused before it runs an
	// instruction (TestUngrantedHostFunctionsCannotEvenBeImported). The
	// sandbox around it mounts no filesystem, exports no environment and
	// allows no hosts, which the escape corpus member measures by *reporting*
	// each attempt rather than merely failing, so a plugin that crashed early
	// cannot be mistaken for containment.
	{ID: "INV-X1", Title: "Plugins touch data only via capability-checked host functions — no ambient authority", Layer: LayerPipeline, TestRequired: true},
	// INV-X2 stays with WP-3.1b: there is no hook running inside a transaction
	// yet, so nothing this host does can partially commit one. Claiming it here
	// would be claiming a property nothing can currently violate.
	{ID: "INV-X2", Title: "Plugin/hook failure never corrupts or partially commits a transaction", Layer: LayerPipeline, Note: "lands with WP-3.1b hook dispatch"},
	{ID: "INV-X3", Title: "Agent/AI writes go through the same command pipeline, permissions, and gates as humans", Layer: LayerPipeline, Note: "lands with WP-3.4 MCP server"},
	{ID: "INV-X4", Title: "No autonomous process can modify invariant-enforcement code, this catalog, or its tests", Layer: LayerPipeline, Note: "lands with Phase 6 self-evolution (ADR-014)"},
	{ID: "INV-X5", Title: "Migration/import writes obey every invariant; bulk paths get batching, not bypasses", Layer: LayerPipeline, Note: "lands with WP-7.x migration factory"},
}

// ProtectedTables returns every table the catalog marks append-only, i.e.
// the tables EnforceAppendOnlyGrants revokes mutation privileges on. Derived
// from the catalog so the grant policy and the invariant list can never
// drift apart.
func ProtectedTables() []string {
	seen := map[string]bool{}
	var tables []string
	for _, inv := range Catalog {
		for _, tbl := range inv.AppendOnlyTables {
			if !seen[tbl] {
				seen[tbl] = true
				tables = append(tables, tbl)
			}
		}
	}
	return tables
}
