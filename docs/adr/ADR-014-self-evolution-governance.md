# ADR-014: Self-evolution governance — the autonomy ladder

**Status:** Accepted · 2026-07-07

## Context
Requirement: the system should learn from its data, build its own features, self-heal, and self-preserve. Uncontrolled self-modification in a system that moves money and computes payroll is an unacceptable risk surface (and would void every auditability claim). Industry patterns (2026): self-healing via monitor→diagnose→remediate agent loops inside guardrails; runtime trajectory inspection before irreversible actions.

## Decision
Adopt the five-level **autonomy ladder** (docs/13): L0 observe/learn → L1 suggest → L2 self-configure metadata (human-approved, revertible) → L3 self-extend via sandboxed WASM plugins (shadow-tested, capability-reviewed) → L4 core improvement proposals through full CI + human merge.

Constitutional constraints, unmodifiable by any autonomous process:
1. Financial invariants (double-entry, immutability, period locks) and their enforcement code.
2. Permission floors, approval-gate machinery, audit-trail writers.
3. The autonomy governance itself (this ladder, kill switches, budgets).
4. Sync correctness properties (no silent loss, server authority).

Mechanisms: every autonomous change is attributable (agent principal), previewable (diff), approved per tenant policy, revertible (versioned packages / staged rollouts with auto-rollback), and audited. Shadow tenants provide production-realistic testing without production risk. Kill switches: per-agent, per-tenant-all-agents, global-autonomy-off; honored at the kernel gateway, not by agent cooperation.

## Rejected
- **Unrestricted self-modification** ("the system rewrites its own core at runtime"): destroys auditability, certifiability (SOC2/GoBD/SOX-adjacent), and the trust that makes an ERP an ERP. Marketing fantasy, liability reality.
- **No autonomy** (suggestions only): wastes the architecture's unique leverage; consultants-in-a-box (L2/L3) is the single biggest cost-killer for users.
- **Autonomy as a separate "AI layer" product**: must be kernel-native or permissions/audit fragment.

## Consequences
- L2/L3 pipelines are product surface, not internal tooling — designed, documented, tested like any module.
- Shadow-tenant infrastructure becomes a hard dependency for Phase 6 (also pays off for upgrade dry-runs and support).
- The repo's own development workflow (agents + CI gauntlet + human merge) is the reference implementation of L4.
