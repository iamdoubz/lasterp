# WP-3.2 — PDKs, registry, and the plugin's public surfaces: decisions

Interpretation of [docs/11-ROADMAP.md](../11-ROADMAP.md) WP-3.2 against
[ADR-007](../adr/ADR-007-plugin-system.md), [ADR-012](../adr/ADR-012-license.md),
[docs/05](../05-PLUGIN-SYSTEM.md), [docs/19](../19-DATA-INTEGRITY.md) §1–2, and what
[WP-3.1a/b](WP-3.1-decisions.md) actually shipped. Written before the code, per CLAUDE.md.

## 0. Scope — this WP is split into two PRs

The roadmap line is "Rust/Go/TS/Python scaffolds, typed bindings codegen, signed bundles, install
flow", plus `/ext/<plugin>/` endpoints moved here by WP-3.1b's plan review, plus the two example
plugins and the tutorial its AC names. Reading it against the code adds two more items nobody
listed: the manifest's **`overlays:`** is refused today with "WP-3.2 (bundle install)" as its
owner, and the Slack-notifier example **cannot exist without an audited outbound HTTP client** —
WP-3.1a refused `capabilities.http` by name and left "the WP that adds an audited client"
unnamed. This is that WP.

That is a sandbox-authority change, a supply-chain format, a CLI, a code generator and two example
plugins in one line. Split, on the same precedent as WP-1.6, WP-2.2, WP-2.3 and WP-3.1, at the
boundary where the *risk* changes:

- **PR-A (WP-3.2a) — the plugin's public surfaces.** What a plugin may do that it could not do
  yesterday: **audited `http.request`** (outbound) and **`/ext/<plugin>/` endpoints** (inbound).
  Both widen the sandbox, so both are hostile-corpus work; neither depends on how a plugin
  arrives. Carries the INV-X1 extension.
- **PR-B (WP-3.2b) — the supply chain and the author.** How a plugin arrives and who can write
  one: signed bundles carrying overlays, `lasterp plugin new|pack|install`, the registry client,
  the language scaffolds, typed bindings codegen, the two example plugins and the tutorial.
  Carries both roadmap ACs ("afternoon-plugin tutorial completes"; "example plugins pass").

The examples land in B, not A, which reopens the objection that moved `/ext/` here in the first
place — *do not build a surface before a caller exists*. A answers it with callers in miniature:
the `/ext/` route and the HTTP client each get a corpus plugin in `kernel/plugins/testdata/` that
exercises them end to end, and B's examples are then the second caller rather than the first.
Designing the route against a real request path is the part that was guesswork before 3.1b
shipped; a polished example is not.

## 1. `http.request`: the sandbox gets a network, and the network is audited

ADR-007 requires outbound calls to be **allowlisted and audited**. Extism's built-in client does
the first and not the second, which is why WP-3.1a left `AllowedHosts` empty and refused `http:`
outright. That stays true — the built-in client is never enabled. `lasterp_http_request` is our
own host function over `net/http`, and it audits every call in `audit_log` under `plugin:<id>` —
method, host, path — in the same vocabulary as every other plugin action.

**The row is written before the socket opens**, which is the only ordering in which no call can
happen unaudited: if the row cannot be written, the request is not made. It is therefore a record
of the *request*, not of the outcome — `audit_log` is append-only (docs/19 §2), so nothing can go
back and add the status. That is deliberate rather than reluctant; a completion row doubles the
volume of what will be the noisiest audit action, and the plugin already receives the status. The
upgrade path is named in the code where the choice was made.

**No bodies, no headers, and no query string.** A plugin that reads `acme_api_key` from the vault
and sends it as a bearer token would otherwise write that secret to the audit log in plaintext,
which is exactly INV-K1 — and API keys live in query strings, so the path is recorded without one.
The audit row records that a call happened and where it went; it is not a proxy log.

Four containment rules, each because the allowlist alone does not hold:

- **The resolved IP is checked, not just the name.** An allowlist entry is a DNS name, and a name
  the plugin's author controls can resolve to `169.254.169.254`, `10.0.0.0/8` or `127.0.0.1`. The
  check runs in the dialer's `Control` hook, which fires after resolution with the address the
  kernel is about to connect to — so DNS rebinding between resolution and connection has no window
  to land in.
- **Redirects are not followed.** A 302 to a non-allowlisted host is how an allowlist is bypassed
  in one hop; the 3xx is returned to the plugin, which may re-request within its own allowlist.
- **HTTPS only.** Plaintext outbound from a server holding tenant data is not a knob.
- **Bounded**: request and response bodies capped, the call deadline is what remains of the
  invocation's wall-clock budget, and it spends from the existing host-call budget.

The private-address rule has one legitimate exception — a self-hoster whose plugin calls an
internal service on the same LAN — so it is a **deployment** dial (`LASTERP_PLUGIN_HTTP_ALLOW_PRIVATE`,
default: refuse), not a per-plugin one. A plugin cannot ask for it; an operator can grant it to the
whole host. The same posture carries the deployment's trust roots (an internal service usually
presents a private CA), which is the pair of settings that belong together. That also gives the
tests their seam honestly, rather than a test-only switch inside a security control: the SSRF test
refuses the loopback destination under the default policy and then *succeeds* against the same
destination under the operator's, so a refusal cannot be mistaken for an unreachable test server.

**Neither surface adds a grant check at install, and that is deliberate.** Object capabilities are
checked against the approver's own grants and `secrets:` against `secret:manage`, because both name
authority the authz vocabulary already carries. "May call api.acme.com" and "may serve /report" do
not exist in that vocabulary, and inventing a permission for them would mean inventing a role
nobody assigns. The power that approves them is `plugin:manage` — which is already the power to
install code that reads whatever the manifest declares. What the administrator gets instead is
visibility: both lists are shown at approval and listed by `GET /api/v1/plugins` afterwards.

## 2. `/ext/<plugin>/`: authenticated, capability-gated, and the plugin is still the actor

The open question WP-3.1b left is *what gates a plugin-declared route*. Answer, in three parts:

**One route pattern, registered once.** `GET|POST /ext/{plugin}/{path...}` resolves the plugin
from the request's tenant at call time. The alternative — mutating the mux when a plugin installs —
produces a route table that cannot be enumerated, and the route-enumeration integrity tests
(`routes_integrity_test.go`, and the INV-K1 sweep that walks every route) depend on being able to
enumerate it. A tenant-varying set of routes hidden behind one stable pattern is enumerable; a
mux that changes under a running server is not.

**The gate is the caller's session plus `plugin:invoke`** — the same capability that already gates
`POST /api/v1/plugins/{id}/call/{fn}`, because it is the same power: running plugin code. No
public/anonymous ext routes (`Public` is an INV-T2 hole reserved for session issuance, and
anonymous inbound webhooks are a different question — they authenticate by signature, not by
capability, and belong to the connector work in docs/07). Writes go through the gateway's
idempotency wrapper like every other write.

**The plugin still runs as its own principal.** The caller's identity is passed to the plugin as
context and grants it nothing: authority is the manifest ∩ the approver's grants, decided at
install, exactly as WP-3.1a settled it. An ext endpoint that could act as its caller would make a
plugin's power vary per request — the thing §3 of WP-3.1-decisions rejected.

The response is clamped, not proxied: status from a small allowed set, `Content-Type` from a short
allowlist, body size capped, no header passthrough, no cookies, no redirects. A plugin that can
set arbitrary response headers on the ERP's own origin has an XSS/CSP surface the sandbox exists
to deny.

Administrator visibility, which the roadmap asks for by name: `GET /api/v1/plugins` lists each
plugin's declared endpoints, so "what does this thing expose" is answerable from the API that
already answers "what was it granted".

## 3. Bundles are a signed tar, not an OCI artifact (PR-B)

ADR-007 says "signed OCI-style bundles". Taken literally that means an OCI registry client, a
media-type scheme and probably a cosign dependency, for a v1 whose job is "one file that carries a
manifest, a module and its overlays, and proves who built it". **OCI-style, not OCI**: a
`.tar.gz` containing `manifest.yaml`, `plugin.wasm`, `overlays/*.yaml` and a detached
`signature.json` (ed25519 over the SHA-256 of a canonical file list), served from any HTTP host
with an `index.json`. Stdlib only — `archive/tar`, `compress/gzip`, `crypto/ed25519` — so the
supply-chain path adds no dependency of its own, which is the right shape for a supply-chain path.
Content addressing is the digest WP-3.1a already records and re-checks on every load; signature
verification attaches to that digest rather than inventing a second identity. If a real OCI
registry is wanted later, the bundle is already a tarball with a digest.

Trust root: a tenant configures the publisher keys it trusts. An unsigned or
untrusted-key bundle is refused at install, not warned about.

## 4. Version solving is the host range, and nothing else yet

docs/05 says "semver, dependency solver". The dependency that exists today is the manifest's
`lasterp:` host range, parsed and *recorded* by WP-3.1a and never enforced. B enforces it — that
is the version solving that has a consumer. Plugin→plugin dependencies have none: no plugin can
call another, so a solver would be resolving edges that cannot exist. A manifest declaring one is
refused by name, in the same style as every other unimplemented field.

**Staged rollout to a sandbox tenant** (docs/05's registry bullet) is likewise deferred: installing
into a test tenant first is something an administrator can do today with two API calls, and an
orchestrator for it is a feature of a fleet manager LastERP does not have.

## 5. Four scaffolds, one of them proven in CI

`lasterp plugin new --lang rust|go|ts|python` emits all four, because a scaffold is a template and
withholding three of them helps nobody. But CI pins **one** toolchain (Go 1.26.5, and
`wasip1/wasm` is in `go tool dist list` — the same route the hostile corpus takes), so only the Go
scaffold is compiled and run end to end in the test suite. Rust, TS and Python scaffolds are
asserted to render, to carry a manifest this host parses, and to pin their PDK version; whether
they compile is the author's toolchain's business. Adding rustc, node's `extism-js` and a Python
wasm toolchain to CI to prove three templates is a dependency in everything but name — the same
argument that kept the hostile corpus off TinyGo.

The tutorial's "afternoon plugin" is therefore written against the Go scaffold, which is the one
the AC can actually execute. Stated plainly in the tutorial rather than implied.

Everything under the PDK and scaffold trees is **Apache-2.0** with per-directory `LICENSE` files
(ADR-012: the ABI boundary is the licensing boundary), which is what `kernel/plugins/abi/`'s
otherwise-empty Apache LICENSE has been holding a place for since WP-0.1.

## 6. Typed bindings come from the effective schema, per tenant

`lasterp plugin bindings --lang go|ts` generates types from a tenant's **effective** schemas —
base plus overlays — because the custom fields are the entire reason a plugin author needs
generated types at all. Go and TS only: those are the two the repo already compiles, and a
generator whose output nothing type-checks is a generator that silently rots.

## 7. Nothing here is reachable by an autonomous process

Unchanged from WP-3.1 §7, and worth re-stating because a registry is a new way for code to enter a
deployment: installing from a bundle still requires an authenticated administrator, still refuses
any capability that administrator does not hold, and now additionally refuses any bundle not
signed by a key the tenant trusts. No host function fetches, installs, or trusts anything. An
autonomous process gains no new door.
