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

	// Event store (INV-E) — enforced as of WP-0.4/0.8.
	{ID: "INV-E1", Title: "Streams are append-only; no UPDATE/DELETE on the events table", Layer: LayerStorage, TestRequired: true, AppendOnlyTables: []string{"events"}},
	{ID: "INV-E2", Title: "Optimistic concurrency: version conflicts are rejected, never silently merged", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-E3", Title: "Events are immutable post-commit; schema evolution via upcasters only", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-E4", Title: "command_id is unique: replay/retry produces exactly-once effects", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-E5", Title: "Projections are pure functions of the log: rebuild(events) ≡ projection", Layer: LayerSentinel, TestRequired: true},

	// Tenancy & access (INV-T) — enforced as of WP-0.3/0.5/0.6/0.8.
	{ID: "INV-T1", Title: "No query path returns another tenant's rows (RLS backstop; zero rows without context)", Layer: LayerStorage, TestRequired: true},
	{ID: "INV-T2", Title: "No write path executes without an authenticated principal and authz decision", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-T3", Title: "Permission floors and approval gates cannot be lowered by overlays/plugins/agents", Layer: LayerPipeline, TestRequired: true},
	{ID: "INV-T4", Title: "Every mutation is attributable: actor, command, timestamp — no anonymous writes", Layer: LayerStorage, TestRequired: true, AppendOnlyTables: []string{"audit_log"}},

	// Sync (INV-S) — Phase 2.
	{ID: "INV-S1", Title: "No acknowledged write is ever lost (RPO 0)", Layer: LayerPipeline, Note: "lands with WP-2.3 sync"},
	{ID: "INV-S2", Title: "Offline commands pass the identical validation pipeline as online writes", Layer: LayerPipeline, Note: "lands with WP-2.3 sync"},
	{ID: "INV-S3", Title: "Client replica converges to server state; divergence is detected and repaired", Layer: LayerSentinel, Note: "lands with WP-2.2 sync"},
	{ID: "INV-S4", Title: "Rejected commands are surfaced to the user; no silent drops", Layer: LayerPipeline, Note: "lands with WP-2.3 sync"},

	// Extension & autonomy (INV-X) — Phase 3 / Phase 6.
	{ID: "INV-X1", Title: "Plugins touch data only via capability-checked host functions — no ambient authority", Layer: LayerPipeline, Note: "lands with WP-3.1 plugin host"},
	{ID: "INV-X2", Title: "Plugin/hook failure never corrupts or partially commits a transaction", Layer: LayerPipeline, Note: "lands with WP-3.1 plugin host"},
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
