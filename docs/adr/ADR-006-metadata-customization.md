# ADR-006: Customization — metadata objects with overlay layers

**Status:** Accepted · 2026-07-06

## Context
"Infinite customization" destroyed by "upgrade hell" is THE recurring ERP failure mode: customers patch core, upgrades break patches, systems freeze in time. Frappe's DocType metadata approach is the best prior art but couples metadata to a specific runtime and mutates core definitions in place.

## Decision
Every business object is defined by a **versioned schema document** (`Object`), and all customization lives in **overlay layers** that are merged at runtime, never edited into core:

```
effective schema = core object (shipped, immutable)
                 ⊕ module extensions (shipped by modules)
                 ⊕ plugin overlays (installed plugins)
                 ⊕ tenant overlays (admin customizations: custom fields,
                    validations, workflows, UI layout, naming series)
```

- Overlays may: add fields, add validations (declarative rules or WASM hooks), adjust UI layouts, add states/transitions to workflows, relabel, hide. Overlays may **not**: remove core fields, weaken core invariants (double-entry, permission floors).
- Custom fields for core objects store in a JSONB column with generated typed accessors and optional expression indexes; fully custom objects get generated tables.
- The metadata engine generates from the effective schema: storage DDL/migrations, validation, REST/gRPC endpoints, list/form UI descriptors, permission matrix, MCP tool schemas, and TypeScript types.
- All metadata is data: exportable as JSON/YAML "customization packages", versionable in git, promotable dev → staging → prod.

## Rationale
- Upgrades replace the core layer only; overlays re-merge. Conflicts (core renamed a field an overlay touches) are detected at upgrade time with a report — not discovered in production.
- One definition feeding storage, API, UI, permissions, and AI tools eliminates the drift that plagues hand-built ERP customizations.

## Rejected
- In-place metadata editing (Frappe-style "customize form" mutating the doctype): upgrade conflicts by construction.
- Code-first customization only: excludes the admin/consultant persona who made Odoo/Frappe successful.

## Consequences
- The metadata engine is the single hardest kernel component; it gets built and tested first (Roadmap Phase 0/1).
- Schema versions + migration plans are first-class objects; every overlay change produces a reviewable diff.
