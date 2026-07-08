# 13 — Self-Evolution: a system that grows, learns, heals, and preserves itself

Dan's requirement: the ERP should learn from its data, implement new features itself, and not be bound by initial source code — self-healing, self-preserving. This is achievable **because** of choices already made (metadata-as-data, sandboxed plugins, agents-as-principals, event log as ground truth), and it is safe **only** with the autonomy ladder below. An ERP that rewrites itself without gates is not the last ERP you'll ever need — it's the last one you'd ever trust.

## The autonomy ladder (each level = capability + gate)

**L0 — Observe & learn (always on).**
The learning substrate: every command, tool call, search, correction, rejection, and approval is signal. Kernel maintains per-tenant learned models (statistical + embedding-based, local, exportable, erasable): account-coding suggestions from history, payment-behavior profiles, duplicate/anomaly detectors, workload patterns, search ranking tuned by clicks. *The system gets measurably better at YOUR business with every transaction — this is the "grows and learns" core, and it needs no code generation at all.*

**L1 — Suggest (always on, human applies).**
Learned signals surface as suggestions with evidence: "You've manually added a 'PO number' note to 34 invoices — add a custom field?" · "This automation would have saved 6 hours last month — enable?" · "Vendor X invoices deviate from their 12-month pattern." Accepted suggestions feed back into L0 (the flywheel).

**L2 — Self-configure (agent builds, human approves, one click to revert).**
Agents author **metadata**: custom fields, views, automations, workflow tweaks, report definitions, dashboard layouts — expressed as customization packages (ADR-006), applied through the same pipeline as human admin changes. Every change: previewed as a diff, approved by an authorized human (tenant may allowlist low-risk classes for auto-apply), versioned, instantly revertible. *This is "implementing new features without touching source code" — most 'features' users ask consultants for are exactly this class.*

**L3 — Self-extend (agent writes plugins, sandbox + review contain them).**
For needs metadata can't express, agents generate **WASM plugins**: code-gen → auto-test against a shadow tenant (fork of real data in an isolated instance) → capability review (the manifest says exactly what it can touch) → human approval → staged rollout with auto-rollback on error-budget breach. The sandbox (ADR-007) means even a badly generated plugin cannot corrupt the ledger or leak data — the blast radius is bounded by construction, not by trusting the generator.

**L4 — Self-improve core (agent proposes, the gauntlet disposes).**
Agents (and users via "wish" reports) file improvement proposals against the product itself; agent-written PRs run the full CI gauntlet (invariant property tests, sync simulation, perf budgets, security suites) and require human maintainer merge. Tenants share opt-in anonymized capability gaps upstream ("47 tenants built lot-tracking overlays → promote to core module"). *The project's own development process is the L4 loop — LastERP is built by agents from day one; this just makes the loop permanent and community-wide.*

**Hard floor at every level:** financial invariants, permission floors, approval gates, and audit trails are **constitutional** — no level of autonomy can modify the enforcement machinery itself. Changes to the constitution are human-governed ADRs. The ledger never learns to bend.

## Self-healing (runtime)

| Failure | Autonomous response |
|---|---|
| Projection drift/corruption | Continuous checksum audit vs event-fold oracle → auto-rebuild projection from event log (projections are disposable by design, ADR-003) |
| Stuck sync client / poisoned outbox command | Quarantine command, sync continues around it, file review task (docs/12) with diagnosis |
| Failing plugin | Circuit breaker → disable hook, notify, file task with captured repro; agent may propose a fix (L3 path) |
| Failing connector mapping (remote API drift) | DLQ + agent diagnosis against remote schema → proposed mapping patch (L2/L3 approval flow) |
| Degrading query performance | Detect from telemetry → propose/apply index within maintenance window (auto-revert if regression) |
| Node/infra failure | Standard k8s + health probes; single-binary mode: supervised restart, WAL recovery |
| Data-quality drift (unbalanced *drafts*, orphan links, stale FX) | Data sentinels scan continuously → auto-fix mechanical issues, task the judgment calls |

Every autonomous repair is audited like any actor's action and produces a post-hoc report. Repairs that recur become L4 improvement proposals — the system files bugs against itself.

## Self-preservation
Continuous verified backups (restore actually tested, automatically, on schedule — an untested backup is a rumor); hash-chained audit trails detect tampering; chaos suite in CI (kill nodes/partition network mid-sync, assert zero acknowledged-write loss); upgrade dry-runs against a shadow tenant before touching production; the kill switch hierarchy (pause one agent / all agents / all autonomy) is a physical constant of the system — always reachable, never overridable by the system itself.

## Honest boundary
"Not bound by initial source code" is true at L2/L3 today (metadata + sandboxed plugins cover the overwhelming majority of real-world feature requests — this is what consultants bill for) and true-with-humans at L4. Fully ungated self-modification of core is excluded on purpose: the moment the referee can rewrite its own rules, every guarantee in docs/04, 06, and 08 becomes a suggestion. The design bet: **bounded autonomy compounds trust; unbounded autonomy destroys it once.**

## Build plan
- **WP-6.1** Telemetry→learning substrate (L0) + suggestion surface (L1). AC: coding-suggestion acceptance rate measured; suggestions carry evidence; erasable per GDPR.
- **WP-6.2** Agent customization pipeline (L2): package diff/preview/apply/revert via MCP tools. AC: agent builds a field+automation+report from an NL request in shadow tenant; revert restores byte-identical metadata.
- **WP-6.3** Shadow tenants (fork-of-production sandbox) — prerequisite for L3/upgrades. AC: fork 10GB tenant < 5 min, fully isolated.
- **WP-6.4** Plugin-generation pipeline (L3): gen→test→review→staged rollout→auto-rollback. AC: hostile-generation suite (agent given a data-exfiltration goal) contained by capability review + sandbox.
- **WP-6.5** Self-healing runtime v1 (projection rebuild, sentinels, quarantine, circuit breakers). AC: chaos suite scenarios auto-recover; every repair audited.
- **WP-6.6** Improvement-proposal loop (L4) + opt-in telemetry upstream. AC: proposal→agent PR→CI gauntlet flow demonstrated on the repo itself.

Decision record: [ADR-014](adr/ADR-014-self-evolution-governance.md).
