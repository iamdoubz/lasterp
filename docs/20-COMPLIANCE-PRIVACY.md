# 20 — Compliance & Privacy: GDPR, CCPA, DoD, and the ISO Hardening Stack

**Founding requirement (Dan, 2026-07-07):** LastERP shall be GDPR, CCPA, and DoD compliant, using standards like ISO to further harden security and data privacy.

Stance: **compliance is architecture, not paperwork.** LastERP ships the technical controls, the evidence automation, and the documentation templates; a tenant's certification then covers *their* deployment and org process. (No software is "GDPR certified" in a vacuum — what we guarantee is that LastERP never makes compliance impossible, and makes most of it automatic.) Extends docs/08; enforced through the Integrity Gauntlet (docs/19).

## Tier 1 — Privacy law: one engine, every jurisdiction

GDPR (EU), CCPA/CPRA (California — broadest US law, covers employee + B2B data), and the 19 other US state laws in force as of 2026 (Virginia-model rights; Maryland strictest on minimization) share a common core. LastERP implements the superset once, as kernel machinery:

- **Data mapping by construction:** PII/sensitivity tags in object metadata (docs/08) mean the system always knows what personal data exists, where, and why — the foundation every regulation demands. Custom fields and plugin data inherit mandatory classification (untagged personal-data fields are a lint error).
- **Processing-purpose & lawful-basis registry:** every PII-bearing object/field maps to declared purposes; the registry auto-generates the Article-30 record of processing activities (RoPA) and CCPA disclosure inventories.
- **DSAR engine (data-subject/consumer rights):** access/portability (full export per subject, machine-readable), correction, deletion, opt-out of sale/sharing/profiling, restriction. Requests are workflow objects (docs/12) with statutory-deadline SLAs (30d GDPR / 45d CCPA), identity verification, and a full evidence trail. Deletion honors accounting reality: CRUD PII is erased; event-sourced records use **crypto-shredding** (per-subject envelope keys destroyed → payload unrecoverable) while preserving the financial skeleton the tax authority requires — the legal-hold/retention hierarchy (statutory retention beats erasure, documented per Article 17(3)) is encoded, not improvised.
- **Consent & preference management:** consent records are first-class, versioned, effective-dated objects (who, what purpose, when, how obtained, withdrawn when).
- **Retention policies as metadata:** per object type + jurisdiction, effective-dated, enforced by kernel jobs; disposal is logged evidence.
- **Data residency:** region-pinned deployments (self-host trivially; hosted offers EU/US/other regions); cross-border transfer inventory auto-generated. Tenant data never trains shared models (docs/13 learned models are per-tenant, exportable, erasable).
- **Breach response:** the integrity sentinels + audit trails (docs/19) feed a breach-assessment workflow with 72-hour-clock tracking (GDPR Art. 33) and notification templates.

## Tier 2 — ISO/SOC hardening stack (the trust vocabulary of commerce)

Target certifications for the project and hosted service; shipped as a **controls matrix** (`compliance/controls.yaml`) mapping every control to the LastERP feature, config, or process that satisfies it, with automated evidence collection:

| Standard | Role | LastERP posture |
|---|---|---|
| **ISO/IEC 27001** | ISMS — the security backbone | Controls matrix maps Annex A to features (RBAC, RLS, encryption, audit, SDLC gates); evidence auto-collected from audit trails + CI |
| **ISO/IEC 27701** (2025 standalone edition) | Privacy management (PIMS) — bridges GDPR/CCPA/state laws | Tier-1 machinery *is* the PIMS technical layer; templates for the management layer |
| **ISO/IEC 27017/27018** | Cloud security / PII in cloud | Hosted-service controls; guidance pack for self-hosters |
| **SOC 2 Type II** | US commercial trust standard | Same controls matrix, TSC mapping; continuous-evidence approach |
| **ISO 22301** (lite) | Business continuity | Backup/restore drills + chaos suite (docs/13/19) are the evidence |

Evidence automation is the differentiator: because every write is attributed, every config change versioned, every backup verified, and every CI run recorded, audit evidence is a query, not a quarterly fire drill. A `lasterp compliance report --standard iso27001` command renders current control status with linked evidence.

## Tier 3 — DoD / US government tier (the hardest customer as design target)

Current landscape: CMMC 2.0 final rule effective Nov 2025; **Phase 2 (Nov 2026) makes third-party Level 2 certification mandatory** in new CUI-handling contracts; Level 2 = NIST SP 800-171 (Rev. 2 today, 110 controls; Rev. 3 tracked); Level 3 adds NIST SP 800-172 for APT defense; clouds handling CUI need FedRAMP Moderate or equivalency.

LastERP's structural advantage: **self-hosting means a defense contractor runs LastERP inside their own certified enclave** — the software must be *capable* of every 800-171 control without dragging its own cloud into the boundary. Deliverables:

- **NIST 800-171/CMMC capability pack:** control-by-control implementation mapping (AC/AU/IA/SC families → RBAC + RLS, hash-chained audit, MFA/WebAuthn + CAC/PIV via OIDC/PIV-capable IdPs, TLS + field encryption), SSP (System Security Plan) template pre-filled with LastERP's implementations, POA&M tracking as work-management objects, CUI marking/handling: sensitivity labels on objects/fields with flow-down to exports, PDFs, and API responses.
- **FIPS 140-3 mode:** build/runtime flag using Go's native FIPS 140-3 validated crypto module (`crypto/fips140` — one of the reasons the pinned modern Go toolchain matters); ships as `lasterp-fips` build target. TLS restricted to approved suites, algorithms constrained system-wide.
- **Hardening profile:** DISA-STIG-aligned configuration baseline (`lasterp harden --profile stig` applies + reports), session controls (timeout, concurrent-session limits, banners), account lifecycle policies (lockout, aging, privileged review), all configurable to 800-171 ODPs.
- **Air-gap capability:** the offline-first architecture (docs/04) extends to *entire deployments*: fully disconnected operation, sneakernet update bundles (signed, verified), no phone-home of any kind in FIPS/air-gap mode. Few ERPs can claim this; for classified-adjacent environments it's decisive.
- **FedRAMP path (hosted, later):** if/when a GovCloud offering exists, it's a separate authorization boundary; self-host-in-your-enclave is the near-term DoD answer.
- Supply chain: SBOM per release (docs/08), signed artifacts, reproducible-build goal, DCO provenance — aligned with EO 14028/SSDF expectations.

## Enforcement & governance
- Compliance controls join the Integrity Gauntlet: session-policy tests, FIPS-mode cipher assertions, DSAR e2e (request → erasure → verify unrecoverable → evidence), retention-job correctness, CUI-label flow-down tests.
- The controls matrix is constitutional-adjacent: changes require human maintainer review (ADR-014 pattern); autonomous processes may *propose* control evidence, never alter control definitions.
- Standing role: a compliance steward reviews quarterly regulatory drift (new state laws, 800-171 Rev. 3 adoption, CMMC phases) and files roadmap items.

## Build plan
- **WP-8.1 Privacy engine** (PII classification enforcement, purpose registry, DSAR workflows, crypto-shredding, retention jobs, consent objects). Lands with Phase 4 (before broad adoption). AC: DSAR e2e across CRUD + event-sourced data; RoPA auto-generated; gauntlet privacy suite green.
- **WP-8.2 Controls matrix + evidence automation + `lasterp compliance report`** (ISO 27001/27701/SOC 2 mappings). Lands with Phase 5 (absorbs WP-5.7). AC: report renders with live evidence links for ≥90% of technical controls.
- **WP-8.3 Government pack v1** (FIPS build, STIG profile, 800-171 mapping + SSP template, CUI labels, air-gap mode). Phase 5/6. AC: FIPS cipher assertions in gauntlet; 800-171 self-assessment score ≥88/110 achievable on documented reference deployment; air-gapped install + signed update e2e.
