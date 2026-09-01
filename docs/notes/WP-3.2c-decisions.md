# WP-3.2c decisions — tenant metadata overlays

WP-3.2c is the second half of ADR-006 that has never existed: `metadata.Merge` has always been
called with **zero** overlays. This WP gives overlays a place to live, a way in, and a request path
that reads them. Written against the code, before it.

Design read: [ADR-006](../adr/ADR-006-metadata-customization.md), [docs/03](../03-DATA-MODEL.md)
§Layer 1, [docs/19](../19-DATA-INTEGRITY.md), [WP-3.2-decisions](WP-3.2-decisions.md) §2b,
[WP-1.11-decisions](WP-1.11-decisions.md).

Invariants touched: **INV-T1** (a tenant's overlay is invisible to another tenant),
**INV-T3** (an overlay may narrow an option set and raise a permission floor, never the reverse),
**INV-T5** (every stored value conforms to the *effective* schema, which is now per-tenant),
**INV-T2/T4** at the overlay admin routes (authorized, attributed).

---

## 1. There is no per-tenant DDL, because there was never going to be

The roadmap entry says this WP needs "per-tenant DDL evolution through WP-1.0a's planner". Read
against the code, it does not. `GenerateDDL` has skipped `FromOverlay` fields since WP-0.5 and the
CRUD engine reads and writes them through the fixed `custom_fields` blob — the physical table is
shared by every tenant *by construction*, which is the whole reason ADR-006 puts custom fields in a
JSON column instead of a column of their own. An overlay that adds a field therefore alters nothing
and plans no migration.

So the WP is: **persist overlays, resolve them per request, and let something author them.** The
DDL half was already paid for, five WPs ago.

## 2. Scope: adding a field to a shipped object. Not new objects.

The AC is "a tenant overlay adding a field to a shipped object round-trips". A *fully custom object*
("fully custom objects get generated tables" — ADR-006) is the part that genuinely needs per-tenant
DDL, a per-tenant route table on the gateway, and an answer to what a replica does with an object
half its fleet has never heard of. It is not in the AC and it is not built here.

An overlay whose document declares an object the host has not registered is **refused by name**,
the same way `overlays:` itself was refused until today — a customization that "installs fine" and
then does nothing is the failure mode the plugin manifest's strict parse exists to prevent.

## 3. One new table, not a column on `object_schemas`

`object_schemas` holds *Object* definitions keyed `(tenant, name, layer, version)`. An overlay is a
different document (add-fields/narrow/permissions, not a whole object) and needs a different key: a
tenant can hold several plugin overlays on one object, and uninstall must be able to remove exactly
one plugin's. Bolting a `source` column onto `object_schemas` costs the same migration and leaves
two document shapes in one `definition` column.

`object_overlays (tenant_id, object_name, layer, source, definition, …)`, PK on all four,
RLS-policied like every other tenant table. `source` is the plugin id, or `''` for the tenant's own
overlay. `definition` is the YAML **verbatim** — the same call `plugins.manifest` and
`automations.definition` make: the stored record is the document an administrator wrote, not this
host's re-marshalling of it, and it is what "customization packages, versionable in git" (ADR-006)
means.

## 4. Merge order is explicit, not alphabetical

ADR-006's stack is core ⊕ module ⊕ plugin ⊕ tenant. Overlays load ordered by an explicit layer rank
then by `source`, in Go — not by `ORDER BY layer` (which happens to be right because `'plugin'`
sorts before `'tenant'`, and a rename would silently reverse the stack). Order is load-bearing:
narrowing composes monotonically only if each layer sees what the one before it left, so the tenant
admin gets the last word by construction rather than by luck.

## 5. The overlay document

```yaml
object: Contact          # which shipped object this overlay targets
add_fields:
  - name: loyalty_tier
    type: enum
    options: [bronze, silver, gold]
narrow_options:
  status: [active]       # subset of core's set — never a superset (INV-T3)
permissions:
  read: [admin, sales]   # superset of core's roles — never a subset (INV-T3)
```

`object:` lives *in* the document rather than in the filename, so an exported overlay is
self-describing and a bundle cannot retarget one by renaming a file.

## 6. No cache, deliberately, and the upgrade path is named

The roadmap entry asks for "cache invalidation across nodes". A cache that must be correct across
nodes needs a per-tenant generation to validate against — which is the same round trip the read it
was avoiding would have cost. So the request path does **one indexed `SELECT` on a table that is
empty for most tenants**, and there is no cache and no invalidation protocol to get wrong.

Measured against the docs/09 gate (`internal/app/perf_test.go`, p95 < 100ms read / 300ms write),
which is where this decision gets checked rather than argued. The upgrade path — a
`tenants.overlay_generation` counter bumped on write, an in-process cache validated against it — is
named in the code at the point the choice was made, and is worth building when the read shows up in
a profile, not before.

## 7. Where per-tenant resolution attaches

Three request paths read schemas; all three become per-tenant:

- **`kernel/api` CRUD routes.** The gateway keeps registering routes once from the boot schema list
  (routes are per-object, and this WP adds no per-tenant objects — §2), but builds the
  `metadata.CRUD` engine *per request* from the resolved schema. `NewCRUD` is a struct wrap; the
  cost is the resolve, not the construction.
- **`/api/v1/meta/objects`.** Already documented as "an INV-T1 read path like any other, not a
  static document" — which was aspirational until now.
- **`/api/v1/sync/*`.** The replica hydrates through the same CRUD engine, so a custom field reaches
  the replica the moment the snapshot does.

**`/api/v1/openapi.json` stays global.** It is the deployment's API description, cached by tooling;
a per-tenant OpenAPI is a different feature with a different consumer. Typed *bindings* are already
generated from `/api/v1/meta/objects` (WP-3.2b, `cmd/lasterp/plugin.go`), which is the tenant-aware
surface — so the thing `kernel/plugins/bindings.go:24` said it was waiting for is what it gets.

## 8. The doctor stops lying

`internal/app/conformance.go` skips overlay fields because "there is no authoring path that can
produce one yet (Merge is called with zero overlays everywhere)". That sentence stops being true in
this WP, and a scanner whose blind spot is documented by an obsolete comment is worse than one with
a known gap. Two fixes, both small:

- the scan resolves each tenant's effective schema **inside** the per-tenant loop, so a *narrowed*
  core enum is scanned against the tenant's set rather than core's (without this, the exact rows a
  narrowing strands are the rows the scanner cannot see);
- overlay enum fields are scanned through the dialect's JSON accessor (`custom_fields::jsonb ->> ?`
  on Postgres, `json_extract(custom_fields, ?)` on SQLite).

## 9. Plugin overlays are validated at install, not at first use

`Install` merges every overlay the bundle carries against the live core object and **refuses the
install** if any fails — a widened option set, a lowered floor, a field colliding with core's or
with another plugin's, an unknown target object. Same rule as the capability check one function
above it: an install that cannot be honoured in full does not happen (INV-T3). `Uninstall` deletes
the plugin's overlay rows in the same transaction as its plugin row, for the reason its role and its
kv go: an overlay left behind is a schema change nobody can see the owner of.

The bundle gains `overlays/*.yaml` entries, inside the existing entry cap and covered by the
existing content digest — so a *swapped overlay* under a valid signature fails the same check a
swapped module does.
