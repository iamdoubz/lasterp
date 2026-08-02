# ADR-020: QR encoding for TOTP enrollment — take `rsc.io/qr`

**Status:** Accepted · 2026-08-01

## Context

WP-1.12's enrollment endpoint issues an `otpauth://` URI **and** a QR code. A QR
encoder is not in the standard library and is not in `go.mod`, so CLAUDE.md's
"no new runtime dependencies without an ADR" rule makes this a decision that has
to be written down either way.

The candidates were `rsc.io/qr`, `github.com/skip2/go-qrcode` (unmaintained
since 2021), and writing one in-tree.

## Decision

**Take `rsc.io/qr`.** `kernel/identity.TOTPQRDataURI` calls
`qr.Encode(uri, qr.M)` and base64-wraps the resulting PNG.

### Why not hand-roll it

An in-tree encoder was the initial proposal, on the reasoning that ADR-019
declined a JOSE library and this should follow. That reasoning does not
transfer, in both directions:

- **ADR-019's argument was about a *parser*.** A JOSE library's risk is that it
  accepts attacker-controlled input and can be talked into accepting the wrong
  thing (`alg: none`, HMAC-with-public-key). A QR encoder parses nothing: it
  takes a string this server generated and emits a bitmap. There is no untrusted
  input and no security-relevant failure mode — the failure mode is "the code
  will not scan", which is loud, local and caught in review.
- **The cost was not comparable either.** ADR-019's stdlib verifier is a few
  hundred lines of comparisons. A conforming QR encoder is Reed–Solomon over
  GF(256), interleaved block layout, alignment-pattern and version-information
  tables, and ISO/IEC 18004 mask penalty scoring — roughly 600 lines that have
  to be *correct*, plus a test harness large enough to prove it (syndrome
  checks, BCH format decode, and a decoder written in the test file, since there
  is no reference implementation to diff against). That is ~1000 lines of
  non-TOTP code inside a TOTP work package, permanently owned by this repo.

Writing it would have optimized for a dependency count while paying in the
currency that actually costs: code to review, maintain and get right.

### Why `rsc.io/qr` specifically

- **Zero transitive dependencies.** `go mod tidy` added exactly one module. The
  supply-chain edge is one node deep, which is the entire concern the
  no-new-deps rule exists to manage.
- **It is Russ Cox's, and it is the encoder `rsc.io/2fa` uses for this exact
  purpose** — TOTP enrollment QR codes. The use case is not adjacent, it is
  identical.
- **It has been stable since 2015.** For a pure function over a fixed ISO
  standard, "no commits recently" is what finished looks like, not abandonment —
  the opposite of what it would mean for a TLS or JOSE library.
- API surface is `Encode(text, level) (*Code, error)` and `Code.PNG() []byte`.
  There is nothing to misconfigure.

Error-correction level **M** (~15%) is what authenticator documentation assumes,
with enough margin for a screen photographed at an angle.

### Delivery: server-side, PNG, data URI in the JSON body

Three sub-decisions, unchanged from the original plan:

- **Server-side rather than client-side.** "Every capability must be reachable
  via API/MCP" — a CLI or MCP client enrolling a user must be able to display a
  QR without reimplementing an encoder. Rendering it in TypeScript would put the
  encoder somewhere the API cannot reach.
- **PNG rather than SVG.** The current CSP is `img-src 'self' data:`, so both
  work and both are inert inside `<img>`. PNG keeps "is a data: SVG a script
  vector" out of every future security review, and `Code.PNG()` is already
  there.
- **A data URI in the response body rather than a second `GET` route.** A route
  serving the image would be a cacheable GET returning a credential, needing its
  own no-store handling and its own authz check. One response, one credential,
  one lifetime.

The QR is rendered with `alt=""` (decorative) beside the base32 secret shown as
selectable text. The secret **is** the accessible equivalent of the image and is
needed anyway for manual entry, so this is both the correct a11y answer and one
that passes the axe-core gate without inventing a description of a bitmap.

## Consequences

- One new module in `go.mod`: `rsc.io/qr v0.2.0`. It is covered by the
  `govulncheck` and SBOM gates WP-1.10 landed, like every other dependency.
- The encoder is not ours to fix. If it ever needs to change (a new
  error-correction level, a different output format), the options are a fork or
  a rewrite — accepted, because neither is likely for a frozen standard.
- Any future QR need (payment codes, device pairing) has an answer already in
  the tree.

## Alternatives rejected

- **`github.com/skip2/go-qrcode`** — more convenient rendering options, but
  unmaintained since 2021 and it pulls in more surface for the same output.
- **In-tree `kernel/qr`** — see above. It remains the right answer for anything
  where the parsing risk is real; it is the wrong one for a pure function over a
  fixed standard.
- **Client-side rendering (a TS QR library in `web/`)** — would put the encoder
  out of reach of API and MCP clients, violating the reachability commandment,
  and trades a Go dependency for an npm one rather than avoiding one.
