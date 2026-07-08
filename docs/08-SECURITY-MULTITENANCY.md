# 08 — Security & Multitenancy

## Identity & access
- **AuthN:** OIDC (any IdP: Entra ID, Okta, Google, Keycloak), SAML for enterprise tier, built-in email+password with TOTP/WebAuthn for solo/team. SCIM provisioning (enterprise).
- **Principals:** users, service principals (integrations, AI agents), device tokens (sync clients). All access through short-lived tokens; refresh bound to device.
- **AuthZ: RBAC + row-level rules.** Roles bundle permissions of shape `(object, action, condition)` where condition is an optional CEL expression over record + actor (e.g., `record.owner == actor.id || actor.team in record.team`). Permission floors from core cannot be lowered by overlays (ADR-006). Field-level read/write masks for sensitive fields (salary, bank details).
- **Approvals:** kernel approval-request service (used by AI gates, payment runs, PO thresholds); N-eyes configurable.

## Tenant isolation (ADR-005)
- Postgres RLS on every tenant table; session `app.tenant_id` set by middleware from the token — never from request params.
- CI gate: the "no-context query returns zero rows" test suite; static check that every new table has tenant_id + RLS policy.
- Per-tenant: rate limits, statement timeouts, storage quotas, connection budgets.

## Data protection
- TLS everywhere; HSTS; modern cipher suites only.
- At rest: disk/volume encryption assumed; field-level encryption (kernel envelope encryption, keys in KMS/age file for self-host) for designated sensitive fields.
- **Client replicas:** SQLite encrypted (SQLCipher/OS keystore-derived key); device registration + remote-wipe token honored at connect (documented limit: an offline stolen device retains data until wipe — encryption is the real control).
- Secrets vault for connector/plugin credentials; secrets never enter logs, events, or plugin memory unread (capability-gated `secrets.get`).
- PII tagging in metadata → drives export redaction, retention policies, GDPR erasure workflow (erasure of PII on CRUD objects; event payload crypto-shredding for event-sourced ones).

## Audit & compliance posture
- Three immutable trails: event store (financial), audit_log (master data), agent_audit (AI). Append-only, exportable, hash-chained per day for tamper evidence.
- Compliance-friendly defaults: double-entry integrity, period close with locks, correction-by-reversal, sequential document numbering per jurisdiction rules, 4-eyes on payments. Full compliance architecture — GDPR/CCPA privacy engine, ISO 27001/27701/SOC 2 controls matrix, DoD tier (NIST 800-171/CMMC, FIPS 140-3, air-gap) — in [20-COMPLIANCE-PRIVACY.md](20-COMPLIANCE-PRIVACY.md).

## Supply chain & platform hardening
- Plugins: signed bundles, capability review at install, sandbox (ADR-007). Dependencies: pinned + `govulncheck`/`npm audit` in CI; SBOM published per release.
- Standard web hardening: CSP, output encoding, parameterized SQL only (enforced by construction — the metadata engine emits parameterized queries), CSRF tokens on browser sessions, upload scanning hooks.
- `SECURITY.md` + coordinated disclosure; secrets scanning in CI.

## Threat model highlights (full doc grows in tools/threat-model.md)
| Threat | Control |
|---|---|
| Cross-tenant leak via app bug | RLS as backstop + CI gates |
| Malicious plugin | WASM sandbox, capabilities, no ambient authority, audit |
| Rogue/compromised AI agent | Role limits, budgets, approval gates, kill switch, dedicated audit |
| Offline device theft | Replica encryption, scoped replicas, remote wipe |
| Sync replay/forgery | Device-bound tokens, command_id dedupe, server revalidation |
| Insider financial fraud | Immutability, 4-eyes approvals, anomaly detection, hash-chained trails |
