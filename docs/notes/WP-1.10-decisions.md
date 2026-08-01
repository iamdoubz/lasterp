# WP-1.10 decisions — Phase-1 review follow-ups

**Status: in progress (2026-07-31).** Branch `wp-1.10`.

WP-1.10 is the Phase-1 counterpart to WP-0.10: the P1 items from
[phase-1-review.md](phase-1-review.md), none of which any feature WP owned. It is
perimeter work — supply chain, hardening, and making a guarantee that is true in
CI also true in a deployment.

## Invariants touched

No new catalog entries and no `TestRequired` flips — all four are already
enforced and tested. What changes is *where* the enforcement is live.

| Invariant | Why it applies |
|---|---|
| **INV-F5** | "Every financially-relevant document posts to GL through its declared template; no direct ledger writes outside the posting pipeline." The `REVOKE INSERT ON events` in `EnforceLedgerPipelineGrants` is what makes this storage-enforced rather than convention-enforced, and today it runs only in tests. Item (d) is entirely about this. |
| **INV-E1** | docs/19 words it as "DB grants **and** triggers make it impossible, not just forbidden". The triggers ship; the grants do not. Item (d) lands the other half. |
| **INV-T4** | Same grants cover `audit_log`. |
| **INV-T2** | Item (e) rewires the password authentication path. It must not regress the fence — `routes_integrity_test.go` is the existing guard and stays. |

## Ambiguities resolved

**1. Two PRs.** PR-A is supply chain, toolchain, lint floor and the doc/config
nits — mechanical, no product behaviour change. PR-B is web hardening, role
separation and the authentication convergence — product code with real tests.

The split is not cosmetic: **PR-A carries a fix for a vulnerability that is
reachable in shipped code** (GO-2026-5970 via `storage.Open`). Holding that
behind a deployment-hardening PR that needs Postgres e2e work would be the wrong
trade. Precedent: WP-1.2 and WP-1.6 both split for far weaker reasons.

**2. CSP is strict, with no configuration knob.** The built bundle
(`web/dist/index.html`) carries no inline script and no inline style — Vite emits
a hashed module script and a stylesheet link, and Tailwind v4 compiles to a CSS
file. So the strict policy actually holds:

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none';
form-action 'self'; frame-ancestors 'none'
```

`img-src` allows `data:` because the metadata renderer may inline small images;
everything else is same-origin only. No knob until a real deployment needs one —
a configurable CSP whose default nobody tests is a policy that decays into
`unsafe-inline` the first time someone hits a wall.

This applies to the Go server, which serves the *built* bundle. `lasterp dev`
serves the client from Vite on its own port, so HMR's inline scripts and
websocket are unaffected.

**3. HSTS is sent unconditionally**, `max-age=31536000`, **without**
`includeSubDomains` and **without** `preload`.

Unconditional because browsers ignore HSTS on a plaintext response by design
(RFC 6797 §8.1), so always sending it is both safe and zero-config — the
alternative (sniffing `X-Forwarded-Proto`) means trusting a header from the edge,
which this product deliberately refuses to do for client IPs already
(WP-1.5: rate limiting keys off `RemoteAddr`, never `X-Forwarded-For`). Applying
a different trust rule to the same class of header for a weaker reason would be
incoherent.

No `includeSubDomains` because LastERP does not know what else lives on its
parent domain, and a self-hoster putting it on `erp.example.com` should not have
their unrelated `wiki.example.com` forced to HTTPS by a default. No `preload`
because that is an irreversible submission to a browser-vendor list and is not a
decision an application server gets to make for its operator. Both are one-line
changes for an operator who wants them, at their edge, where the domain-wide
knowledge lives.

**4. Headers are applied in `internal/app`, not `kernel/api`.** CSP depends on
what is being served — the bundle — and `kernel/api` deliberately knows nothing
about the web client (it is the metadata-driven API surface, usable headless).
The composition root is where the two are joined, so it is where the policy
covering both belongs. The middleware wraps the whole handler, so an API-only
deployment with no bundle still gets every header.

**5. `govulncheck ./...` runs as a plain gate with no waiver file.** After the
dependency bumps the reachable finding is gone; the remaining `x/crypto`
advisories are in code we do not call, which govulncheck already reports without
failing. Building a waiver mechanism before there is anything to waive is
speculative — if a future advisory is reachable and unfixable, that is the moment
to add one, with an expiry, and a reviewer looking at it.

**6. `pnpm audit` gates on production dependencies and reports on the rest.**
CI runs `pnpm audit --prod --audit-level=low` as a **blocking** gate — nothing
vulnerable may enter the shipped bundle — and `pnpm audit` across everything as a
**non-blocking** report.

The reason for the asymmetry: all 9 current advisories are in dev tooling
(vitest, vite, esbuild, postcss, brace-expansion) and none reaches a user. A
blocking gate on the full tree makes the build hostage to whichever test-runner
transitive dependency published an advisory that morning, which is how audit
gates get disabled. We still fix them — the vitest bump that clears the critical
advisory is in PR-A — but "developer tooling has an advisory" and "we shipped a
vulnerability" are different severities and should fail differently.

**7. SBOM-per-release is OUT OF SCOPE, and this is a deliberate reduction.**
docs/08 specifies "SBOM published per release". There is no release pipeline to
publish one from: `.github/workflows/` contains only `ci.yml`, there is no
`.goreleaser.yml`, and no tag has ever been built. Generating an SBOM artifact on
every CI run would satisfy the letter and miss the point — an SBOM's value is
being attached to a released artifact with its digest.

Building the release pipeline inside a follow-ups WP would be a second WP wearing
a trenchcoat. **Flagged for whichever WP creates the release workflow**; the two
should land together. The other two thirds of the docs/08 sentence
(`govulncheck`, `npm audit` in CI) are delivered here.

**8. `lasterp harden` and `lasterp doctor`.**

- `lasterp harden --app-role <name>` runs as the **owner/migration** role and
  applies `EnforceAppendOnlyGrants` + `EnforceLedgerPipelineGrants`, optionally
  creating the role first (`--create-role`, password from
  `LASTERP_APP_PASSWORD` — never a flag, same reasoning as
  `LASTERP_BOOTSTRAP_PASSWORD`).
- `lasterp doctor` runs as **whatever role the app uses** and reports posture.

`doctor` probes with `has_table_privilege(current_user, 'events', 'INSERT')` and
friends — a read-only catalog query — rather than attempting a forbidden write.
A diagnostic that works by trying to violate an invariant is a diagnostic that
will one day succeed.

I am deliberately claiming the name `lasterp doctor`, which WP-10.1 also plans to
use. One command with one check that WP-10.1 extends is better than inventing
`lasterp check` now and merging two commands later.

**9. The authentication convergence goes through the provider, not around it.**
`PasswordTOTPProvider.Authenticate` absorbs the logic — including the
dummy-bcrypt timing equalizer that currently exists only in the HTTP handler —
and `internal/app/sessions.go` calls it. The reverse (delete the provider, keep
the handler) was rejected because WP-1.9 routes OIDC through `AuthProvider`, so
deleting it would leave one provider implementing an interface with no other
implementation, which is the same smell from the other direction.

The equalizer belongs in the provider on its own merits: any caller
authenticating a password wants the unknown-user path to cost the same as the
wrong-password path, and a caller that has to remember to add it is a caller that
will forget.

**10. The tax/FX reference-data read API is the designated cut.** It is the
largest of the items filed as "nits" and the only one that adds API surface
rather than fixing something. It goes last in PR-B; if PR-B grows, it is the
first thing dropped to its own follow-up rather than the thing that delays the
hardening.

## Deferred (flagged)

- **SBOM per release** — see §7; belongs with the release pipeline.
- **Cross-replica rate limiting** — still per-process (noted since WP-0.6);
  arrives with Valkey/ADR-016 at scale.
- **CSP `report-uri`/`report-to`** — no endpoint to collect reports and no
  storage decision for them; revisit when there is an observability story.
- **Automated dependency updates** (Dependabot/Renovate) — the gate this WP adds
  tells you when you are behind; automating the bump is a separate call about
  bot-authored PRs against a DCO-enforced repo.
