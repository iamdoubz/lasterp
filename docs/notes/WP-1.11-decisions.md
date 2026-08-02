# WP-1.11 decisions — Metadata field validation + schema descriptors

**Status: implemented (2026-08-01).** Branch `wp-1.11`. Plan approved by the repo owner
before code; this file records the interpretation and what shipped against it.

WP-1.11 closes the ADR-006 gap [WP-1.6 §5](WP-1.6-decisions.md) called "the one finding
that outlives this WP" and [phase-1-review.md](phase-1-review.md) re-confirmed:
`kernel/metadata` has no value validation at all, and `enum` has no option list, so
`POST /api/v1/account {"type":"banana"}` lands in the chart of accounts. It also absorbs
the UI-descriptor deferral from [WP-1.5 §2](WP-1.5-decisions.md) — field order, grouping
and widget overrides are the same schema surface, and splitting them across two WPs would
mean two schema-version bumps on every shipped object.

It is scheduled before Phase 2 because **WP-2.2 generates the client replica schema from
this metadata**. Whatever the value domain of a field is, it has to be *in the metadata*
for the replica generator to see it — see §4.

## What is actually broken (verified in this worktree, at 2eb1c1c)

1. `CRUD.validate` (`kernel/metadata/crud.go:58`) checks required-ness and nothing else.
2. **`Update` never calls it.** `Create` validates; `Update` (crud.go:222) goes from
   `authz.Authorize` straight into the transaction. So a `PUT` that nulls a required field
   or writes a bool into an int column is unvalidated today even for required-ness. This
   half of the finding is not recorded anywhere; it is in scope here.
3. `FieldEnum` has no options and falls to the `default:` branch in both `columnType`
   (ddl.go:28) and `scanRecord` (crud.go:405) — an enum column is `TEXT` and free text.
4. **Five** enum fields ship, not six: `Contact.kind`, `Account.type`, `Period.status`,
   `Invoice.status`, `Receipt.status`. The original brief said six; the sixth was a match in
   a test file. `TestEveryRegisteredEnumFieldDeclaresOptions` pins the count at five against
   what the running server registered, so a sixth has to be added deliberately.
5. `Account.type` and `Contact.kind` are the two reachable through generic CRUD
   (`crudObjects()`, `internal/app/app.go:179`). `Contact.kind` is the worse of the two:
   `contacts.CreateContact` validates against `validKinds`, but the generic route bypasses
   it entirely and no report surfaces the result. `Account.type` at least lands in
   `modules/reporting`'s `unclassified` bucket.
6. There is **no production overlay authoring path**. `metadata.Merge` is called with zero
   overlays in every module; `Overlay` is exercised only in `kernel/metadata/*_test.go`.
   That bounds what the narrowing rule in §3 can be proven against — see §9.
7. Unrelated drift found while reading the catalog: **`INV-F8` was registered in
   `kernel/integrity/catalog.go` (since WP-1.6a) but missing from
   `docs/19-DATA-INTEGRITY.md` §1.** Pre-existing — *not* introduced by this WP — and
   restored here, since this WP edits that list anyway to add INV-T5.

## Invariants touched

| Invariant | Status | Why it applies here |
|---|---|---|
| **INV-T5** *(new)* | register, `TestRequired: true` | "Every stored field value conforms to its object's effective schema — declared type and declared option set; no write path stores a value outside it." Nothing in the catalog covers schema conformance of *data*. §5 explains why it is a T and not a new family. |
| **INV-T3** | wording widened, no `TestRequired` change | "Permission floors and approval gates cannot be lowered by overlays, plugins, or agents." The option-set ceiling in §3 is the identical constitutional rule applied to data, so INV-T3's line grows "…and declared value domains cannot be widened". Narrowing/widening tests tag INV-T3. |
| **INV-T2** | must not regress | Validation runs *after* `authz.Authorize`, never before. An unauthorized caller must get 403, not a 422 that tells them which values are legal. Create already orders it correctly; Update must keep that order when validation is added to it. |
| **INV-T1 / INV-T4** | must not regress | Validation is pure and in-process; it adds no query path and no write path. Normalization (§2) changes what is written, so the `audit_log` `changes` blob must record the **normalized** value, not the raw input — otherwise the audit trail disagrees with the row. |
| **INV-F4** | reinforced | The per-type rules refuse `float64` for `money`/`decimal`/`percent` outright. JSON floats are exactly how a float gets into a money path from the API edge; this is the first place that can say no. |
| **INV-S2** | enabled, not claimed | "Offline commands pass the identical validation pipeline as online writes." Achievable only if the validation rules are *metadata* rather than module code. This WP makes them metadata; WP-2.3 claims the invariant. |
| **INV-X5** | placement rationale | "Bulk paths get batching, not bypasses." The validator therefore sits in the CRUD engine, not in the HTTP handler, so the migration factory and MCP tool surface inherit it without opting in. |

## Ambiguities resolved

### 1. Options live on the field, and an enum without options is a schema error

`Field` gains `Options []string` (`yaml:"options,omitempty"`). `Field.validate()` — already
shared by `Object.Validate` and `Merge`, so a core schema and an overlay are held to the
same bar — gains:

- `Options` non-empty **iff** `Type == FieldEnum`. An enum with no options is
  `ErrInvalidObject`; a non-enum with options is `ErrInvalidObject`.
- Options are non-empty, unique, and have no leading/trailing whitespace.

Making a bare `enum` a *parse* error rather than a lint is what converts AC-1 ("every enum
field in a shipped module declares its options") from a checklist into a property: a module
that forgets fails `ParseObject` inside its own `Register()`, and `lasterp serve` refuses to
boot. There is no state in which a shipped enum field is unconstrained.

**Rejected:** a separate `enums:` block at object level, or a reference to a shared
`enum_registry`. Both are indirection for a list of three strings, and a shared registry
would need its own overlay/narrowing story. If two objects ever need the same set, they can
repeat it; if that becomes painful, that is the evidence for a registry.

### 2. Type validation is per-`FieldType`, and it normalizes

New file `kernel/metadata/value.go`. `CRUD.validate` becomes:

```go
// validated returns a normalized copy of rec, or ErrValidation.
// partial=true (Update) checks only the keys present in rec.
func (c *CRUD) validated(rec Record, partial bool) (Record, error)
```

Create calls `validated(rec, false)`, Update calls `validated(changes, true)`. Both then use
the **normalized** record for the SQL values, the returned `Record`, and the audit diff.

Normalization is not optional polish. `metadata.Record` is `map[string]any` decoded straight
from JSON (`decodeRecord`, `kernel/api/gateway.go:475`), so a JSON `int` arrives as
`float64` and is handed to the driver as `float64` for an `INT` column. `modernc.org/sqlite`
takes it via type affinity; Postgres does not. That is an adapter-conformance divergence
sitting latent behind the fact that no generic-CRUD object has an int field yet. Accepting
`float64` and storing `int64` is the fix.

The rules, one per `FieldType` (the table is exhaustive by construction — see §8):

| Type | Accepts | Normalizes to | Refuses |
|---|---|---|---|
| `text` `long_text` `rich_text` `phone` `duration` `file` | `string` | as-is | non-string |
| `email` | `string` parsed by `net/mail.ParseAddress` | as-is | anything unparseable |
| `int` | `int`/`int32`/`int64`, or `float64` with zero fractional part | `int64` | `5.5`, `"5"`, `bool` |
| `bool` | `bool` | as-is | `"true"`, `1` |
| `decimal` `percent` | `string` matching `^-?\d+(\.\d+)?$` | as-is | any `float64` |
| `money` | `string` of integer minor units `^-?\d+$` | as-is | any `float64`, any fractional string |
| `currency` | `string` accepted by `money.Lookup` | canonical ISO code | unknown code |
| `date` | `time.Time`, or `string` parsed by `time.DateOnly` | `time.Time` (UTC) | other layouts |
| `datetime` | `time.Time`, or RFC 3339 `string` | `time.Time` (UTC) | other layouts |
| `enum` | `string` present in `Options` | as-is | out-of-set, non-string |
| `link` | `string` (an id) | as-is | non-string |
| `json` | any JSON value; a `string` must itself parse as JSON | as-is | invalid JSON string |
| `address` | `map[string]any`, or a JSON-object `string` | as-is | scalar |
| `table` | `[]any` | as-is | non-slice |
| `computed` | nothing — a non-nil value is refused | — | any client-supplied value |

`nil` and `""` skip the type check; absence is the required-field check's business, and a
`""` on an optional field is how the client clears it (`submittable`, `render.tsx:295`).

Three notes on the edges:

- **`currency` uses `money.Lookup`**, which adds a `kernel/metadata → kernel/money` import.
  `kernel/money` imports only `kernel/storage` and `kernel/tenancy`, so there is no cycle.
  A shape check (`^[A-Z]{3}$`) would accept `XYZ`; CLAUDE.md already routes every money
  concern through `kernel/money` and this is the currency registry's whole job.
- **`money` is a placeholder shape**, matching `columnType`'s own admission that a
  first-class two-column money representation is still owed (ddl.go:28). No shipped object
  has a `money` field. When that representation lands, this row moves with it.
- **`computed` refuses a value.** `render.tsx:48` already drops computed fields from forms,
  and `objectSchema` marks the kernel's system columns `readOnly` but not computed fields —
  so the API currently advertises them as writable. Accepting-and-ignoring would let a client
  believe it wrote something. Nothing uses `computed` today, so this costs nothing now and is
  the right default when something does. (`fieldSchema` should mark them `readOnly` in the
  same change.)

**Referential validation of `link` is out of scope** and flagged: checking that the target id
exists needs a per-target query inside the write transaction and a story for self-references
(`Account.parent → Account`). That is a foreign-key feature, not a type check.

### 3. Overlays narrow option sets; the merge grows one operation, not a mutation

`Overlay` gains `NarrowOptions map[string][]string` and the merge gains
`ErrOptionSetWidened`. Rules, applied per overlay in layer order:

1. The named field must exist in the effective schema so far and be `FieldEnum` — otherwise
   `ErrOverlayConflict` (reusing the existing error: "your overlay refers to a field that
   isn't what you think it is").
2. The proposed set must be a **subset** of the current effective set. Any member outside it
   is `ErrOptionSetWidened`.
3. The proposed set must be non-empty. Narrowing a required enum to nothing makes the object
   unwritable, which is a broken tenant, not a customization.
4. Narrowing composes monotonically: layer *n+1* narrows layer *n*'s already-narrowed set and
   can never restore a value an earlier layer removed.

**Why a new operation instead of letting an overlay re-declare the field.** `AddFields` is
add-only and a name collision is `ErrOverlayConflict` by design; making a collision mean
"redefine" is exactly the in-place metadata mutation ADR-006 rejects (its "Rejected" section
names Frappe's customize-form). A dedicated narrowing verb says *constrain*, and it can only
express constraint — there is no shape of `NarrowOptions` that widens.

**How it composes with the existing merge, which is the interesting part.** `Merge` already
holds one bound on overlays: `Permissions` must be a **superset** of what an earlier layer
required (`ErrPermissionFloorLowered`). Options are the mirror image — an overlay must stay
**within** the core set. The two look opposite and are the same rule:

> The core layer's declaration is a bound no later layer may escape. Permissions are a
> **floor** (roles are a capability; removing one takes access away that core promised),
> options are a **ceiling** (values are a domain; adding one admits data core never
> declared). Overlays may always move *away from* more capability and *toward* less data.

Both directions of the same rule are ADR-006's "overlays may not weaken core invariants",
which is why the tests tag INV-T3 rather than minting a second constitutional entry. A
widened option set is a tenant admitting `Account.type: "banana"` by policy — the data
equivalent of granting itself a role core withheld.

**Overlay-added enum fields** need no special case: they come through `AddFields`, so
`Field.validate()` already forces them to declare options, and those options become the
ceiling for any later layer.

### 4. Enforcement lives in the metadata engine. Not in the storage layer. Deliberately.

The written answer the brief asks for.

**Enum options and per-type validation are enforced at the metadata engine only** — the
CRUD write path, docs/19 enforcement layer 3 (command pipeline). No `CHECK` constraints are
generated. Four reasons, in order of weight:

1. **WP-2.2 reads metadata, not DDL.** The client replica is generated from the same schema
   documents this WP extends. If the option set lives in a Postgres `CHECK`, the replica
   generator cannot see it, and INV-S2's "offline commands pass the identical validation
   pipeline" becomes two pipelines that agree by coincidence. This is the mechanical reason
   WP-1.11 blocks WP-2.2, and the reason to lead with.
2. **A shared table cannot express a per-tenant domain.** `GenerateDDL` produces one
   physical table for every tenant (that is what `tenant_id` + RLS is for). §3 lets a tenant
   overlay narrow an option set. A `CHECK` could therefore only encode the widest, core set —
   it would enforce a *strictly weaker* rule than the engine while looking authoritative.
   A constraint that is right about less than the code above it is worse than no constraint.
   `TestNarrowedOptionBindsTheWritePath` is the demonstration: two engines over one table,
   disagreeing about what is legal, both correct.
3. **SQLite cannot add one.** `ALTER TABLE … ADD CONSTRAINT` does not exist there. WP-1.0a
   already hit this wall and made widen/loosen steps Postgres-only (evolve.go:146). A
   Postgres-only `CHECK` is a semantic divergence between dialects, which the adapter
   conformance gate exists to prevent.
4. **Overlay fields live in `custom_fields` JSON.** There is no portable `CHECK` on a blob
   key, so overlay-added enums would be engine-validated regardless — two enforcement stories
   for one field type.

This is *not* an abandonment of layer 2. Required-ness stays enforced at both layers
(`NOT NULL` from `GenerateDDL`, plus the engine check), and that asymmetry is deliberate:
`NOT NULL` is expressible identically on both dialects, tenant-independently, at
`CREATE TABLE` time. Nothing about an option set is.

### 5. INV-T5 is a new catalog entry, in the T family

The type/option conformance rule has no home in the catalog. INV-T3 covers *who may change
the rules*; nothing covers *whether stored data obeys them*. docs/19 §1 is explicit ("New
modules MUST register their invariants here; CI fails if an invariant has no tagged tests"),
and WP-1.6a set the precedent by adding INV-F8.

**INV-T5 — Every stored field value conforms to its object's effective schema (declared type
and declared option set); no write path stores a value outside it.** Layer: `LayerPipeline`.
`TestRequired: true`.

"Tenancy & access" is an imperfect family name for a schema-conformance rule, and it is still
the right one: T is where the cross-cutting write-path rules already live — INV-T2 ("no write
path executes without an authz decision"), INV-T4 ("every mutation is attributable"). "No
write path stores a value outside the declared domain" belongs beside them. F is financial, E
is the event store, S is sync, X is extensions. A sixth family would also need
`kernel/integrity/catalog_test.go`'s `INV-[FETSX][0-9]+` regex widened, which is
invariant-enforcement code under CODEOWNERS — a bigger blast radius than the naming buys.

**Alternative, if the maintainer prefers:** fold conformance into INV-T3 and mint nothing.
Cheaper in catalog churn, but it conflates "overlays cannot widen the rules" with "data obeys
the rules", and the second is what WP-2.2 needs to point at. Recommending the new entry.

### 6. Existing out-of-set rows: nothing is rewritten, and they stay editable

The second written answer the brief asks for. Four parts.

1. **No backfill and no rewrite.** Nothing can know what `type: "banana"` was meant to be,
   and a guess writes a wrong number into a chart of accounts. Coercing it to a valid value
   would be the same silent misstatement WP-1.6 §5 built the `unclassified` bucket to avoid.
2. **The read path does not validate.** `scanRecord` stays as it is. A non-conforming row
   reads back through `GET`, the renderer, and every report exactly as it does today.
   Validating on read would make a bad row *unreadable*, and therefore unfixable.
3. **Update validates only touched keys** (`partial=true`, §2). An existing bad row can still
   be edited on every other field; the only thing refused is writing a new bad value. Fixing
   the row is one `PUT` with a valid value. This is the single most important behavioral
   consequence of the partial-validation choice, and it is why partial is correct rather than
   merely convenient.
4. **Reports keep `unclassified`.** `modules/reporting.classify` is not touched. Its comment
   gets a pointer to this WP (the root cause is fixed for *new* writes; the bucket now covers
   rows written before the fix and any future gap), but the bucket itself stays. Deleting a
   mitigation because the cause is fixed is how the cause comes back.

**Scoped addition: `lasterp doctor` learns to find them.** Today nothing enumerates
non-conforming rows, and only `Account.type` is visible at all (via `unclassified`);
`Contact.kind` is invisible. `doctor` already exists (WP-1.10d) and already answers "is this
running deployment actually what CI proves". Adding a schema-conformance scan keeps that
contract:

- Enumerate registered core schemas from `object_schemas` (module-agnostic — no new exported
  module accessors), parse each, take the CRUD ones with enum fields.
- Per tenant (`SELECT id FROM tenants`, which carries no RLS policy since it *is* the tenant
  registry), inside `tenancy.WithTenant` so RLS is satisfied rather than bypassed, run one
  grouped count per enum column of values outside the option set.
- Report as a distinct `schema_conformance` key in the JSON, **not** as `Findings`, and
  **do not change the exit status**. `doctor`'s non-zero exit means "role separation is not in
  effect" and is used as a readiness gate; a deployment with one legacy account is
  misconfigured data, not an unhealthy process, and conflating them would make operators
  disable the gate.

If a reviewer wants this out of scope, it is separable — the behavioral answer (parts 1–4)
stands without it. It is included because the repo's pattern (WP-1.6 §5, WP-1.10d) is to
refuse to ship a rule nothing can observe.

### 7. UI descriptors: three attributes, presentation only, never DDL

`Field` gains `Order int`, `Group string`, `Widget string` (all `omitempty`).

- **`Order`** — fields sort by `(Order, declaration index)`, stably. Unset (`0`) therefore
  means "keep schema order", so every existing schema renders identically and an overlay can
  place a new field between two core ones. **`Order` never reorders columns.**
  `selectColumns` and `scanRecord` walk `schema.Fields` in lockstep, and `PlanEvolution`
  diffs on it; the effective schema's field slice keeps declaration order and the sort happens
  only in the presentation projection (`toMetaObject`) and in the client. Reordering a form is
  not a migration and must never plan one.
- **`Group`** — a free string. `ObjectForm` renders one `<fieldset>` + `<legend>` per group in
  first-appearance order, ungrouped fields first. `fieldset/legend` is the accessible
  primitive, which matters with the axe-core gate live. List and detail views ignore groups;
  a list has no room for them.
- **`Widget`** — a closed set of exactly two overrides, each with a type rule:
  `textarea` (valid on `text`/`long_text`/`rich_text`) and `radio` (valid on `enum`). Anything
  else is `ErrInvalidObject`. Two is not a placeholder — both are real, both render, both get
  tested. A widget vocabulary invented ahead of a caller is the thing WP-1.5 §2 declined to do
  and it would be no better done here.

**An overlay may set descriptors only on the fields it adds.** ADR-006 does say overlays may
"adjust UI layouts", which implies re-ordering a *core* field from an overlay — but that needs
a mutate-in-place merge operation the engine deliberately does not have, and there is no
production overlay authoring path to exercise it (§ "What is actually broken", item 6).
Deferred to the customization-package WP (Phase 3), flagged not dropped. Following the ADR;
noting the shortfall.

### 8. Exhaustiveness on both sides of the wire

The client already has `assertNever` in `FieldControl` so a new kernel `FieldType` is a
compile error rather than an unrendered form (render.tsx:274). The server has no equivalent —
`columnType` and `scanRecord` both have `default:` branches, which is precisely how
`FieldEnum` became free text.

`validateValue` gets the server-side mirror: a `map[FieldType]rule` declared alongside
`validFieldTypes`, and a unit test asserting the two maps have identical key sets. Adding a
`FieldType` without a validation rule then fails the build. This is the structural fix behind
the specific bug — the specific bug was a missing case in a `default:`-terminated switch.

### 9. Schema versions must be bumped, and the evolution plans no DDL

`SaveObjectSchema` refuses a different definition at an existing version
(`ErrSchemaVersionConflict`), so adding `options:` to a module's YAML **requires a version
bump** or `lasterp serve` fails to boot against any existing database:

| Object | Version | Enum field gaining options |
|---|---|---|
| `Account` | 2 → 3 | `type`: asset, liability, equity, income, expense |
| `Period` | 1 → 2 | `status`: open, closed |
| `Contact` | 2 → 3 | `kind`: customer, vendor, both |
| `Invoice` | 2 → 3 | `status`: draft, posted |
| `Receipt` | 1 → 2 | `status`: draft, posted |

Each is a descriptor-only diff: no column type, required flag or index changes, so
`PlanEvolution` emits zero steps, `ddl` is empty, and `ApplyDDL` records the version and runs
no statements — the storage-equivalent path WP-1.0a built. An `evolve_test.go` case pins that
(a descriptor-only diff must never plan DDL), because the failure mode if it regresses is a
surprise `ALTER` on every upgrade.

**No new SQL migration is needed** — nothing changes in a kernel table. The count stays at
0038.

**Option values stay in step with the module constants** (`ledger.AccountAsset`,
`contacts.KindCustomer`, `invoicing.StatusDraft`) by a test asserting the parsed schema's
`Options` equal the module's constant set, not by interpolating the YAML. The YAML stays
readable and drift stays impossible. The module-level checks (`contacts.validKinds`,
`ledger.CreateAccount`'s type check) are kept: they are a typed error at a typed call site,
and deleting them would change those functions' contracts for no gain now that they agree
with the engine by test.

### 10. Enum values are data, but their labels are UI strings

The `<select>` shows option values. In German, `Contact.kind: customer` should read *Kunde*.
The existing precedent is `schema.field.<Object>.<field>` (messages.ts:62), so options extend
it: **`schema.option.<Object>.<field>.<value>`**, falling back to `humanize(value)` when the
pack has no key — same totality rule as `labelFor`.

Keys are added for the two objects the client actually renders (`Account.type` ×5,
`Contact.kind` ×3 = 8 keys), matching how `schema.field.*` covers only Account and Contact
today. `messages.ts` and `packs/de.json` key sets must match exactly (`i18n.test.ts:101`).

`FieldControl` needs the object name and a `Labeler` to build the key; both are already in
hand at the one call site (`ObjectForm.tsx`). They go in as optional props so the component
stays renderable without them.

## Implementation plan

### Server — `kernel/metadata`

| File | Change |
|---|---|
| `schema.go` | `Field.Options/Order/Group/Widget`; `validWidgets` closed set + per-type applicability; `Field.validate()` rules from §1 and §7. |
| `value.go` *(new)* | `validateValue(f Field, v any) (any, error)` — the §2 table as `map[FieldType]rule`; normalization; `ErrValidation` details naming field and reason. |
| `crud.go` | `validate` → `validated(rec, partial) (Record, error)`; `Create` uses it (unchanged position, after authz); **`Update` starts calling it** with `partial=true`; both write the normalized values and the normalized audit diff. |
| `overlay.go` | `Overlay.NarrowOptions`; `ErrOptionSetWidened`; the §3 merge rule. |

### Server — everything downstream

| File | Change |
|---|---|
| `kernel/api/openapi.go` | `fieldSchema(FieldType)` → `fieldSchema(Field)`, emitting `"enum": [...]` for enum fields (ADR-009: OpenAPI regenerates in the same PR). |
| `internal/app/meta.go` | `metaField` gains `options`, `order`, `group`, `widget`; `toMetaObject` emits fields sorted by `(order, index)` so a dumb consumer is right by default. |
| `internal/app/harden.go`, `cmd/lasterp/main.go` | §6 `doctor` schema-conformance scan; exit status unchanged; usage/doc comments updated. |
| `modules/ledger/schema.go` | `options:` on `Account.type` and `Period.status`; `accountVersion` 2→3, Period 1→2. |
| `modules/contacts/contacts.go` | `options:` on `Contact.kind`; `contactVersion` 2→3. |
| `modules/invoicing/schema.go`, `receipt.go` | `options:` on both `status` fields; Invoice 2→3, Receipt 1→2. |
| `modules/reporting/report.go` | Comment on `TypeUnclassified` points at this WP. The bucket stays. |
| `kernel/integrity/catalog.go` | Register INV-T5; widen INV-T3's title. |

### Web

| File | Change |
|---|---|
| `src/api/index.ts` | `MetaField` gains `options?: string[]`, `order?: number`, `group?: string`, `widget?: "textarea" \| "radio"`. |
| `src/meta/render.tsx` | `orderedFields()`; `editableFields`/`listFields` compose over it (so routes need no change for AC-4); `enum` → `<Select>` over `field.options`; `radio` and `textarea` widget overrides; `optionLabel()` using the §10 key. |
| `src/routes/ObjectForm.tsx` | `<fieldset>`/`<legend>` grouping; pass `object.name` + labeler to `FieldControl`. |
| `src/i18n/messages.ts`, `src/i18n/packs/de.json` | 8 `schema.option.*` keys, identical key sets. |

### Docs

`docs/03-DATA-MODEL.md` (document `options`/`order`/`group`/`widget` in the schema shape) ·
`docs/19-DATA-INTEGRITY.md` (INV-T5; INV-T3 wording; **restore the missing INV-F8 line**) ·
this file.

## Tests, mapped to acceptance criteria

**AC-1 — every enum field in a shipped module declares its options; an out-of-set write is
refused on both dialects.**

| Test | Where | Tag |
|---|---|---|
| `TestEnumWithoutOptionsIsRefused`, `TestNonEnumWithOptionsIsRefused`, `TestOptionsMustBeUniqueAndNonEmpty` | `kernel/metadata/schema_test.go` | unit |
| `TestOutOfSetEnumWriteIsRefused` — create **and** update, Postgres **and** SQLite | `kernel/metadata/value_integrity_test.go` | `integrity`, INV-T5 |
| `TestPostAccountWithUnknownTypeIsRejected` — `POST /api/v1/account {"type":"banana"}` → 422 problem+json (the finding, verbatim, over HTTP) | `internal/app/validation_integrity_test.go` *(new)* | `integrity`, INV-T5 |
| `TestEveryRegisteredEnumFieldDeclaresOptions` — read `object_schemas` after boot, parse, assert | `internal/app/validation_integrity_test.go` | `integrity`, INV-T5 |
| `TestSchemaOptionsMatchModuleConstants` — §9 drift guard | each module's `*_test.go` | unit |

**AC-2 — a wrong-typed value for each `FieldType` is refused.**

| Test | Where | Tag |
|---|---|---|
| `TestValidateValue` — table-driven over all 21 types × valid/invalid, plus a completeness assertion that the rule map and `validFieldTypes` have identical key sets (§8) | `kernel/metadata/value_test.go` | unit |
| `TestIntNormalizesJSONFloat` — `float64(5)` → stored and read back as `int64(5)`; `5.5` refused; on both dialects (the latent adapter divergence, §2) | `kernel/metadata/value_integrity_test.go` | `integrity`, INV-T5 |
| `TestUpdateValidatesTouchedFieldsOnly` — a row holding an out-of-set value is still updatable on other fields; nulling a required field is refused (the §6.3 and the never-called-`validate` bug) | `kernel/metadata/crud_test.go` | unit |
| `TestAuditRecordsNormalizedValue` — the diff blob matches the row | `kernel/metadata/crud_test.go` | unit, INV-T4 |
| `FuzzValidateValue` — malformed input may be rejected, never panics (docs/19 fuzzing row) | `kernel/metadata/value_test.go` | unit |
| `TestUnauthorizedWriteIsNotAValidationOracle` — bad value + no permission → 403, never 422 | `internal/app/validation_integrity_test.go` | `integrity`, INV-T2 |

**AC-3 — an overlay narrowing an option set is accepted; one widening it is refused.**

| Test | Where | Tag |
|---|---|---|
| `TestOverlayNarrowsOptionSet`, `TestOverlayWideningOptionSetIsRefused` (`ErrOptionSetWidened`), `TestNarrowingComposesAcrossLayers`, `TestNarrowNonEnumIsRefused`, `TestNarrowToEmptySetIsRefused` | `kernel/metadata/overlay_test.go` | unit |
| `TestNarrowedOptionBindsTheWritePath` — a value in the core set but outside the tenant-narrowed set is refused by `CRUD.Create`, on both dialects (proves the ceiling binds writes, not just the merge) | `kernel/metadata/overlay_integrity_test.go` *(new)* | `integrity`, INV-T3 |

**AC-4 — the renderer drives field order from the schema rather than declaration order.**

| Test | Where | Tag |
|---|---|---|
| `orderedFields` sorts by `(order, index)`, stable on ties; `listFields`/`editableFields` inherit it | `web/src/meta/render.test.tsx` | vitest |
| `enum` renders a `<select>` with exactly the schema's options, labelled and required-wired | `web/src/meta/render.test.tsx` | vitest |
| `widget: textarea` on a text field renders a textarea; `widget: radio` on an enum renders a radio group; an absent widget falls back to the type default | `web/src/meta/render.test.tsx` | vitest |
| grouped fields render one `<fieldset>`/`<legend>` per group | `web/src/routes/ObjectForm` test | vitest |
| `/api/v1/meta/objects` reports `options`/`order`/`group`/`widget` and emits fields in order | `internal/app/meta_integrity_test.go` | `integrity` |
| The Account form's `type` is a `<select>`; the invoice lifecycle still passes; axe-core stays green | `web/e2e/invoice.spec.ts` | Playwright |

**Cross-cutting**

| Test | Where |
|---|---|
| `TestEveryRequiredInvariantHasATaggedTest` passes with INV-T5 registered | `kernel/integrity/catalog_test.go` (existing gate) |
| `TestDescriptorOnlyDiffPlansNoDDL` — §9 | `kernel/metadata/evolve_test.go` |
| OpenAPI emits `enum` for enum fields | `kernel/api/openapi_test.go` |
| Message/pack key sets still match | `web/src/i18n/i18n.test.ts` (existing gate) |
| Full gauntlet on SQLite + the touched packages on Postgres (`-p 1` locally, Docker contention) | `go test -count=1 -tags integrity ./...` |

## Known rough edges (accepted for v1, named so they are not discovered)

1. **Nothing can author an overlay in production.** `Merge` takes zero overlays everywhere.
   The narrowing rule is proven by unit + integrity tests only — exactly the status
   `ErrPermissionFloorLowered` has had since WP-0.5. It is a rule waiting for its authoring
   surface, not a rule that works today.
2. **`link` fields are not checked for referential existence.** A `parent` pointing at a
   deleted account is still accepted. That is a foreign-key feature.
3. **A tenant that has already narrowed an option set gets no help if core later removes a
   value** from the set. Core removing an option is a destructive schema change with the same
   shape as dropping a column, and `PlanEvolution` has no story for it. Out of scope; the
   engine will simply refuse writes of the removed value.
4. **Ordering is presentation-only, so two clients could disagree** if one sorts and one does
   not. Mitigated by the server emitting sorted fields as well as the `order` attribute.
5. **`money` validation is a placeholder shape** and moves when the two-column money
   representation lands (§2).

## Deferred (flagged, not forgotten)

- **Overlay-driven descriptor changes on core fields** (re-order/re-group a shipped field from
  a tenant overlay) — needs a mutate-in-place merge operation; Phase 3 customization packages.
- **Declarative validation rules beyond type/options** (min/max, regex, cross-field) — ADR-006
  lists "add validations" as an overlay capability. This WP delivers the type/domain floor;
  rule expressions are their own design with a WASM-hook escape hatch (ADR-007).
- **`link` referential integrity** (rough edge 2).
- **A shared enum registry** if repetition ever justifies one (§1).
- **Wiring the §6 conformance scan into a real sentinel** (docs/19 layer 4, WP-6.7) rather than
  a `doctor` subcommand an operator has to run.
