# LastERP

*The last ERP anyone will need to build — or buy.*

A free, open-source, AI-first, offline-capable ERP. Fast by default, infinitely customizable without forking, and built to scale from 5 users to 50,000 concurrent users without data loss.

**Why it exists:** SAP is expensive and unmaintainable. IFS is rigid. QuickBooks is unreliable and nickel-and-dimes. Nothing modern is simultaneously open source, offline-first, plugin-extensible, and AI-native. LastERP is.

## Document index

| Doc | Purpose |
|---|---|
| [docs/00-VISION.md](docs/00-VISION.md) | Product vision, principles, non-goals |
| [docs/01-ARCHITECTURE.md](docs/01-ARCHITECTURE.md) | System architecture overview |
| [docs/02-TECH-STACK.md](docs/02-TECH-STACK.md) | Chosen stack and rationale summary |
| [docs/03-DATA-MODEL.md](docs/03-DATA-MODEL.md) | Metadata object system + core entities |
| [docs/04-SYNC-ENGINE.md](docs/04-SYNC-ENGINE.md) | Offline-first sync design |
| [docs/05-PLUGIN-SYSTEM.md](docs/05-PLUGIN-SYSTEM.md) | WASM plugin architecture |
| [docs/06-AI-INTEGRATION.md](docs/06-AI-INTEGRATION.md) | AI-first / MCP-native design |
| [docs/07-INTEGRATIONS.md](docs/07-INTEGRATIONS.md) | Connector framework (SAP CPQ, Salesforce, etc.) |
| [docs/08-SECURITY-MULTITENANCY.md](docs/08-SECURITY-MULTITENANCY.md) | AuthN/Z, tenancy, audit |
| [docs/09-SCALABILITY-DEPLOYMENT.md](docs/09-SCALABILITY-DEPLOYMENT.md) | 5 → 50k users, self-host to cloud |
| [docs/10-MODULES.md](docs/10-MODULES.md) | Functional modules (GL, AR/AP, CRM, payroll, tax) |
| [docs/11-ROADMAP.md](docs/11-ROADMAP.md) | Phased build plan with agent work packages |
| [docs/12-WORK-MANAGEMENT.md](docs/12-WORK-MANAGEMENT.md) | Tasks/docs/goals — the ClickUp replacement, linked to the ledger |
| [docs/13-SELF-EVOLUTION.md](docs/13-SELF-EVOLUTION.md) | Learning, self-configuring, self-healing, self-preserving design |
| [docs/14-COMPETITIVE-PAIN-POINTS.md](docs/14-COMPETITIVE-PAIN-POINTS.md) | Where every incumbent fails → design priorities |
| [docs/15-API-DEVELOPER-PLATFORM.md](docs/15-API-DEVELOPER-PLATFORM.md) | OpenAPI 3.1 API, MCP server details, portal, Postman/SDKs, third-party auth |
| [docs/16-MIGRATION-FACTORY.md](docs/16-MIGRATION-FACTORY.md) | AI-assisted migration from SAP, IFS, NetSuite, QuickBooks & friends |
| [docs/17-LOCALIZATION-ACCESSIBILITY.md](docs/17-LOCALIZATION-ACCESSIBILITY.md) | i18n, country/compliance packs, e-invoicing mandates, WCAG 2.2 AA |
| [docs/18-BANKING-FINANCIAL-INTEGRATION.md](docs/18-BANKING-FINANCIAL-INTEGRATION.md) | ISO 20022, statements, payment rails, open banking, reconciliation |
| [docs/19-DATA-INTEGRITY.md](docs/19-DATA-INTEGRITY.md) | **The paramount requirement:** invariant catalog + Integrity Gauntlet |
| [docs/20-COMPLIANCE-PRIVACY.md](docs/20-COMPLIANCE-PRIVACY.md) | GDPR/CCPA privacy engine, ISO 27001/27701/SOC 2 controls, DoD/CMMC/FIPS tier |
| [docs/21-REPORTING-DASHBOARDS.md](docs/21-REPORTING-DASHBOARDS.md) | Metrics layer, one-click reports, no-code role-based live dashboards |
| [docs/22-DEPLOYMENT-TOPOLOGIES.md](docs/22-DEPLOYMENT-TOPOLOGIES.md) | Failover, load balancing & sizing tiers from <100 to 50k users |
| [docs/adr/](docs/adr/) | Architecture Decision Records |
| [CLAUDE.md](CLAUDE.md) | Instructions for Claude Code agents building this |

## The commandments (read before writing code)

0. **Data integrity is paramount.** No feature, AI enhancement, or plugin can be allowed to ruin data integrity — proven by the exhaustive Integrity Gauntlet on every change, not promised by review (docs/19).
1. **The ledger is append-only.** Financial state is derived from immutable events. Corrections are reversing entries, never edits.
2. **Everything is an API.** Every UI action maps to a public, versioned API call. No hidden endpoints.
3. **Customization is data, not forks.** Custom fields, objects, workflows, and UI live in metadata overlays that survive upgrades untouched.
4. **Offline is not a mode, it's the default.** The client works from a local SQLite replica; the network is an optimization.
5. **The server is the referee.** Sync is server-authoritative. Clients propose mutations; the server validates, accepts, rejects, or rebases.
6. **Plugins are untrusted.** All third-party code runs sandboxed (WASM) with declared, user-approved capabilities.
7. **AI is a user, not a bolt-on.** Agents act through the same APIs, permissions, and audit trail as humans — via MCP.
8. **Tenant isolation is enforced by the database**, not by application discipline (Postgres RLS + session context).
9. **Sane defaults, infinite dials.** Every knob has a default that works for a 5-person company out of the box. Modular and composable by default: use only the pieces you want (CRM alone, invoice OCR alone…) — the kernel solver knows and shows what each piece needs (ADR-018).
10. **Speed is a feature.** p95 < 100ms for interactive reads, < 300ms for writes, measured continuously in CI.
11. **The system improves itself — inside the fence.** Learning, agent-built customizations, and self-healing are core capabilities; financial invariants, permission floors, and audit machinery are constitutional and no autonomous process may touch them (docs/13, ADR-014).
12. **Compliance is architecture, not paperwork.** Privacy rights (GDPR/CCPA and kin), ISO-grade security controls, and DoD-tier hardening (800-171/CMMC, FIPS, air-gap) are built-in capabilities with automated evidence — never a bolted-on module (docs/20).
