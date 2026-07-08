---
description: Pick up a LastERP Work Package by ID (e.g. /wp 0.4)
---

You are picking up Work Package WP-$ARGUMENTS for LastERP.

Follow this exact sequence:

1. **Orient.** Read README.md (the commandments), then find WP-$ARGUMENTS in docs/11-ROADMAP.md. Confirm its phase is unblocked per the "Build order at a glance" dependency rules — if WP-0.8 is not merged and this is a module WP, STOP and say so.
2. **Read the design.** Read every doc and ADR the WP entry links, plus docs/19-DATA-INTEGRITY.md (always). List the INV-* invariants your WP touches.
3. **Resolve ambiguity in writing.** If anything is underspecified, write docs/notes/WP-$ARGUMENTS-decisions.md stating your interpretation and why. Do not stall; do not relitigate ADRs — if you disagree with an ADR, note it in the decisions file and follow the ADR anyway.
4. **Plan first.** Present an implementation plan: files, interfaces, test list mapped to each acceptance criterion, invariants registered. Wait for approval before writing code.
5. **Tests before/with code.** Every acceptance criterion gets a test before you consider the WP done. Invariant-bearing code gets property tests tagged with INV IDs. Storage-touching code runs the conformance suite on Postgres AND SQLite.
6. **Verify.** Run lint + the full test suite + the Integrity Gauntlet locally. All green or the WP is not done. NEVER weaken, skip, or delete a failing invariant test — fix the code or escalate in the PR description.
7. **Ship.** One branch `wp-$ARGUMENTS`, conventional commits, one PR: description links the WP, lists each AC with pass/fail status, notes any decisions file, and (for authz/sync/plugins/payments work) includes a threat-notes section.

You do not merge your own PR.
