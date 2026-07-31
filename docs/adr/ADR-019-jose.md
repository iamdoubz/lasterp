# ADR-019: OIDC ID-token validation — stdlib JOSE verifier, no new dependency

**Status:** Accepted · 2026-07-31

## Context

WP-1.9 adds an OIDC `AuthProvider`. WP-0.3 deferred it precisely here
(docs/notes/WP-0.3-decisions.md §1): validating an ID token needs a JOSE
implementation, and CLAUDE.md forbids a new runtime dependency without an ADR.
The roadmap entry for WP-1.9 makes that ADR a deliverable.

What actually has to happen to an ID token: split the compact JWS, pick the
signing key out of the IdP's JWKS by `kid`, verify the signature, then check
`iss`/`aud`/`exp`/`iat`/`nonce`/`sub`. The signature step is the only part with
a library question — the rest is JSON and comparisons either way.

The candidates were `github.com/coreos/go-oidc/v3` (+ its
`github.com/go-jose/go-jose/v4` dependency), go-jose alone, `golang-jwt/jwt/v5`,
or stdlib.

## Decision

**No JOSE library. `kernel/oidc/jose.go` verifies ID tokens directly over
`crypto/rsa`, `crypto/ecdsa`, `crypto/sha256` and `encoding/json`**, under these
constraints:

1. **The accepted algorithms are a closed map** of `RS256`, `PS256`, `ES256` to
   verify functions. Nothing else is reachable.
2. **There is no HMAC code path and no `none` code path** anywhere in the
   package — not a rejected branch, an absent one. The two classic JWS
   vulnerabilities (`alg: none`, and `alg: HS256` verified with the IdP's public
   key as the shared secret) are therefore unrepresentable rather than guarded.
3. **The key is selected from the JWKS by `kid`, never from the token's `alg`**,
   and the JWK's own `kty` must match the algorithm family or verification
   fails. A token cannot nominate its own key type.
4. The verifier is fed only JWKS keys fetched from the discovery document of the
   configured issuer over TLS. There is no caller-supplied-key API to misuse.

Signature verification is *defense in depth* here, not the only control: LastERP
is a confidential client using the authorization-code flow with PKCE, so the ID
token arrives over TLS directly from the token endpoint with client
authentication. OIDC Core §3.1.3.7 point 6 permits skipping signature validation
entirely in exactly that situation. We verify anyway — see Rationale — but the
consequence of a hypothetical bug in the verifier is bounded by the fact that
the token never came from the browser.

Deliberately out of scope for this package: JWE (encrypted ID tokens), signing
(LastERP issues opaque session tokens, not JWTs — WP-0.3 §3), and any
front-channel flow that would make signature verification load-bearing.

## Rationale

- **The dependency does not carry its weight.** `go-oidc` + `go-jose` exist
  mostly to do the parts we would write anyway (discovery, claims, caching) plus
  a large surface we must never use: JWE, symmetric algorithms, key management,
  signing, embedded `jwk`/`x5c` header key resolution. The part we genuinely
  need is a few hundred lines, and the excluded surface is where the historical
  CVEs in this space live.
- **This is not hand-rolled crypto.** Every primitive is stdlib —
  `rsa.VerifyPKCS1v15`, `rsa.VerifyPSS`, `ecdsa.Verify`. What we write is
  envelope parsing and algorithm dispatch, and algorithm dispatch is the exact
  thing we want to read in one screen rather than trust.
- **Repo precedent.** RFC 6238 TOTP (WP-0.3 §4), the migration runner (WP-0.2),
  the PDF renderer (WP-1.4), the XLSX writer (WP-1.6b) and the ECB XML parser
  (WP-1.1) were all taken over a dependency on the same reasoning. TOTP is the
  closest analogue: also security-relevant, also spec-defined, also verified
  against the RFC's own test vectors.
- **Testability.** RFC 7515 appendix A.2/A.3 publish complete signed examples;
  the negative cases (`none`, alg confusion, `kid` mismatch, tampered payload)
  are the tests that matter and they are ours to write either way.

Rejected — skipping signature validation under §3.1.3.7. It is spec-legal and
would be the smallest possible implementation, but it permanently forecloses
front-channel flows, and "we do not check the signature" is a sentence that
would have to be defended in every future security review of this codebase. The
verifier is small enough that buying that back is not worth the paragraph.

## Consequences

- `kernel/oidc` stays a leaf package with zero non-stdlib imports; `go.mod` is
  unchanged by WP-1.9.
- Any future need for JWE, symmetric signing, or key rotation beyond
  `kid`-lookup-with-refetch reopens this ADR rather than being bolted on.
- IdPs that sign with algorithms outside RS256/PS256/ES256 (e.g. EdDSA) fail
  closed with a clear error. Adding one is a small map entry plus its own test
  vectors, but is a deliberate change, not configuration.
- The verifier is invariant-adjacent security code: it belongs under CODEOWNERS
  review, like `kernel/authz` and `kernel/integrity`.
