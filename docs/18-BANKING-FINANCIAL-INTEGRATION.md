# 18 — Banking & Financial System Integration

The deep-dive behind docs/07 §L2.5/L2.7 and module M4 (docs/10). Banking is the highest-frequency integration an ERP has — statements arrive daily, payments leave weekly — and the one where errors cost real money. Design stance: **bank-agnostic adapters over a kernel banking service; files always work; agents never move money without gates.**

## 1. Kernel banking service (fixed interfaces, pluggable rails)

```
kernel/banking/
  StatementProvider   — pull/receive account statements & intraday feeds
  PaymentRail         — submit payment instructions, track status lifecycle
  BankConnection      — credentials (vaulted), capabilities, health
  MatchEngine         — statement line ↔ ledger document reconciliation
```
Implementations ship as connectors (docs/07 framework: OAuth/credential vault, retries, DLQ, health dashboards). A tenant can run file-based with Credit Union A and API-based with J.P. Morgan simultaneously.

## 2. Statement ingestion (money in view)

**Formats, priority order:**
- **ISO 20022 camt.053** (end-of-day) + **camt.052** (intraday) + **camt.054** (debit/credit notifications) — the present and future; camt.053 V08 replaces MT940/942, with the SWIFT coexistence window closing **November 2026** — LastERP is ISO-20022-native from day one, legacy formats are converters.
- **Legacy, fully supported:** MT940/MT942 (SWIFT), **BAI2/BTRS** (US cash management, incl. the new RTP/instant-payment codes), OFX/QFX, CAMT-flavored national dialects, and a **CSV mapping wizard** (AI-suggested column mapping via the docs/16 profiler machinery) for the long tail of small banks.
- Ingestion paths: SFTP/host-to-host drop, EBICS (DACH standard), open-banking aggregator APIs, manual upload. All land in an immutable `BankTransaction` staging store (raw payload retained for audit).

**Open banking / aggregators:** one `StatementProvider` adapter per aggregator — Plaid, GoCardless Bank Account Data, Tink, Yapily, and direct PSD2/FDX (US §1033) bank APIs where practical. Aggregators are optional accelerators, never dependencies: every capability has a file-based fallback (a credit union with no API still works).

## 3. Payment initiation (money out — the dangerous direction)

**Formats/rails:**
- **pain.001** (credit transfers; V09+, replaces MT101/legacy domestic formats) and **pain.008** (SEPA direct debits) as the canonical outbound formats; **pain.002** status messages consumed for accept/reject tracking.
- **US:** NACHA ACH files, Fedwire, **FedNow/RTP** instant payments (via bank/provider APIs — instant rails are API-first, no file mode).
- **Europe:** SEPA SCT + SCT Inst, SDD core/B2B. **UK:** BACS, Faster Payments, CHAPS. Others as country packs (docs/17 pattern).
- Cross-border: SWIFT CBPR+ MX messages via the bank's channel; LastERP generates compliant structured party/remittance data (structured addresses etc. — mandatory post-Nov-2026).

**Delivery paths:** file download for portal upload (always works) → SFTP/EBICS submission → direct bank payment APIs / payment-ops providers (Modern Treasury-style, Finzly-style gateways) as certified connectors.

**The EndToEndId discipline (what makes reconciliation nearly free):** every outbound payment carries a LastERP-generated `EndToEndId` (and `UETR` where applicable) persisted on the payment document. When it survives to the camt.053 entry — which ISO 20022 guarantees end-to-end — matching is a database join, not a heuristic. Remittance info structured (`Strd`) wherever the rail allows.

**Controls (non-negotiable, kernel-enforced):**
- Payment runs are event-sourced, approval-gated (4-eyes minimum by default, N-eyes configurable by amount tier), and idempotent (a re-submitted file cannot double-pay — file-level + instruction-level dedupe).
- Bank detail changes on vendors trigger out-of-band verification workflow + cooling period (top fraud vector).
- **Agents can prepare and propose payment runs; execution always requires human approval** — hard floor from ADR-014's constitution; no tenant setting weakens it below single-human approval.
- Positive pay / payee-match file generation where banks support it; sanctions/denied-party screening as a pluggable pre-submission hook (provider adapters; screening data is a compliance-pack concern).
- Full lifecycle audit: created → approved(by) → submitted(channel, file hash) → acknowledged(pain.002) → settled(camt) → reconciled.

## 4. Reconciliation engine (where trust is won daily)

- **Deterministic passes first:** EndToEndId/UETR join → payment/invoice reference parse (structured remittance, virtual account numbers, OCR-line national schemes) → exact amount+date+counterparty.
- **Assisted pass:** learned matcher (docs/13 L0 substrate) proposes matches with confidence + evidence (fee-adjusted amounts, partial payments, batched settlements, FX-converted amounts); accountant confirms in the reconciliation workbench; every confirmation trains the tenant's matcher.
- **Splits & exceptions:** batch settlements (one bank line = N invoices — PSP payouts), partial payments, over/underpayment tolerance rules (auto-write-off within policy), unmatched lines age into a work-management queue (docs/12) with SLA.
- PSP payout reconciliation (Stripe/Adyen/GoCardless): connector fetches payout composition reports; engine explodes one settlement line into constituent charges/fees/refunds and posts fees per posting template automatically.
- Bank reconciliation state is a projection — rebuildable, auditable, per-account close discipline (statement balance must equal ledger cash account per period before period close).

## 5. Treasury surface (v1-light)
Multi-account cash position dashboard (all connected accounts, all currencies, base-currency view via ADR-013 rates), short-horizon cash forecast from AR/AP aging + recurring items (learned-model assisted, clearly labeled as forecast), intercompany transfers as paired journals. Deep treasury (pooling, in-house bank) = future module or plugin territory.

## 6. MCP tools (per docs/06 governance)
`import_statement`, `reconcile_bank_statement` (propose matches), `explain_unmatched_line`, `prepare_payment_run` (draft only), `cash_position`, `forecast_cash`. Execution-class tools (`submit_payment_run`) exist but are approval-gated always.

## Build plan (expands WP-4.2)
- **WP-4.2a Statement ingestion:** camt.053/052/054 + MT940/942 + BAI2 + OFX parsers (golden-file suites from real anonymized samples), CSV wizard, staging store. AC: parser conformance corpus green; malformed-file fuzzing never corrupts state.
- **WP-4.2b Reconciliation engine + workbench UI.** AC: seeded corpus (incl. batch/partial/FX cases) ≥85% auto-match precision 100%-safe (no false auto-match above tolerance); unmatched queue flow e2e.
- **WP-4.2c Payment initiation:** pain.001/pain.008 + NACHA generation (golden-file + bank validator conformance), approval gates, EndToEndId lifecycle, pain.002 consumption. AC: double-submission test cannot double-pay; 4-eyes enforced in e2e.
- **WP-4.2d Aggregator + PSP connectors:** one open-banking aggregator + Stripe payout reconciliation reference implementations. AC: sandbox e2e — statement in via API, invoice paid, payout exploded and posted.
- **WP-5.9 Instant rails + EBICS + screening hook:** FedNow/RTP/SCT Inst via provider APIs, EBICS channel, sanctions-screening plugin interface. AC: instant-payment status lifecycle e2e in provider sandbox.
