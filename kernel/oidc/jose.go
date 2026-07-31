// SPDX-License-Identifier: AGPL-3.0-only

package oidc

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// This file is the whole of LastERP's JOSE surface (ADR-019): enough to verify
// a signed ID token from a configured issuer, and deliberately nothing else.
//
// Two properties are structural rather than checked, because the historical JWS
// vulnerabilities are both failures to hold them:
//
//   - There is no HMAC verifier and no "none" verifier anywhere in this package.
//     A token asking for either does not hit a rejection branch — it fails to
//     find a verifier at all, the same way "RS999" does. The classic attack of
//     re-signing a token with alg=HS256 using the issuer's public key as the
//     shared secret has no code to reach.
//   - The key is chosen from the JWKS by "kid", never from anything the token
//     asserts about it, and the JWK's own "kty" must match the algorithm family.
//     A token cannot nominate its own key or its own key type.
//
// There is intentionally no exported entry point taking a caller-supplied key:
// keys come from the discovery document's JWKS over TLS (oidc.go) or not at all.

// ErrInvalidToken is returned for every reason a token is not acceptable:
// malformed, unknown algorithm, unknown key, bad signature, or a claim that
// does not check out. Undifferentiated on purpose — the caller turns this into
// one opaque authentication failure, and a caller that could tell "bad
// signature" from "wrong audience" would eventually leak the difference.
var ErrInvalidToken = errors.New("oidc: invalid token")

// algVerifier is one accepted signature algorithm. The map of these is the
// complete list of what this package will verify; adding an entry is the only
// way to widen it, and is a reviewed change (ADR-019).
type algVerifier struct {
	// kty is the JWK key type this algorithm requires. A JWK of any other
	// type is not a candidate, however well its kid matches.
	kty string
	// verify reports whether sig is a valid signature by pub over digest.
	// It must return false — never panic — for a key of the wrong concrete
	// type, since the JWKS is remote input.
	verify func(pub any, digest, sig []byte) bool
}

var algs = map[string]algVerifier{
	"RS256": {kty: "RSA", verify: verifyRS256},
	"PS256": {kty: "RSA", verify: verifyPS256},
	"ES256": {kty: "EC", verify: verifyES256},
}

func verifyRS256(pub any, digest, sig []byte) bool {
	k, ok := pub.(*rsa.PublicKey)
	if !ok {
		return false
	}
	return rsa.VerifyPKCS1v15(k, crypto.SHA256, digest, sig) == nil
}

func verifyPS256(pub any, digest, sig []byte) bool {
	k, ok := pub.(*rsa.PublicKey)
	if !ok {
		return false
	}
	// SaltLengthEqualsHash is what RFC 7518 §3.5 specifies for PS256; the
	// permissive "auto" salt length would accept signatures the JWA profile
	// does not allow.
	return rsa.VerifyPSS(k, crypto.SHA256, digest, sig, &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	}) == nil
}

func verifyES256(pub any, digest, sig []byte) bool {
	k, ok := pub.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return false
	}
	// A JWS ECDSA signature is the fixed-width concatenation r||s (RFC 7518
	// §3.4), not the ASN.1 DER encoding ecdsa.VerifyASN1 expects. Length is
	// checked exactly: a short signature left-padded by SetBytes would verify
	// a different (r,s) than the signer produced.
	if len(sig) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(k, digest, r, s)
}

// jwk is one JSON Web Key, restricted to the fields this package uses. Members
// for algorithms it does not implement (symmetric "k", certificate chains
// "x5c", encrypted-key parameters) are ignored rather than parsed: an unusable
// key is simply not a candidate.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// signingKey is a JWKS entry parsed into a usable public key.
type signingKey struct {
	kid string
	kty string
	alg string // the JWK's own "alg", if it declared one; "" otherwise
	pub any
}

// keySet is a parsed JWKS.
type keySet struct {
	keys []signingKey
}

// parseJWKS parses a JWKS document. Keys it cannot use — unsupported types,
// malformed parameters, keys marked for encryption — are skipped rather than
// failing the whole set, because an IdP legitimately publishes keys for
// purposes we do not implement, and one of them must not take down login. A
// document that yields no usable key at all is an error.
func parseJWKS(b []byte) (*keySet, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("oidc: parse JWKS: %w", err)
	}
	ks := &keySet{}
	for _, k := range doc.Keys {
		// "use" is optional, but when present anything other than signature
		// use disqualifies the key — an encryption key must never verify a
		// token.
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil {
			continue
		}
		ks.keys = append(ks.keys, signingKey{kid: k.Kid, kty: k.Kty, alg: k.Alg, pub: pub})
	}
	if len(ks.keys) == 0 {
		return nil, errors.New("oidc: JWKS contains no usable signing key")
	}
	return ks, nil
}

func (k jwk) publicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64uint(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64uint(k.E)
		if err != nil {
			return nil, err
		}
		// A modulus below 2048 bits is not something a current IdP should be
		// signing with, and accepting one silently would make the weakest key
		// in the set the security of the whole login path.
		if n.BitLen() < 2048 || !e.IsInt64() || e.Int64() < 3 {
			return nil, errors.New("oidc: unusable RSA key")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, errors.New("oidc: unsupported EC curve")
		}
		// P-256 coordinates are exactly 32 bytes with leading zeros retained
		// (RFC 7518 §6.2.1.2); a short field is a malformed key, not one to
		// pad.
		x, err := b64bytes(k.X, 32)
		if err != nil {
			return nil, err
		}
		y, err := b64bytes(k.Y, 32)
		if err != nil {
			return nil, err
		}
		// crypto/ecdh's NewPublicKey does the on-curve check (the modern
		// replacement for elliptic.Curve.IsOnCurve) over the SEC1 uncompressed
		// encoding, 0x04 || x || y. A point off the curve is a malformed key,
		// and accepting one is how invalid-curve attacks start.
		point := make([]byte, 0, 1+len(x)+len(y))
		point = append(append(append(point, 4), x...), y...)
		if _, err := ecdh.P256().NewPublicKey(point); err != nil {
			return nil, errors.New("oidc: EC key is not a valid P-256 point")
		}
		return &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}, nil
	default:
		return nil, errors.New("oidc: unsupported key type")
	}
}

// candidate returns the key to verify with: the one whose kid matches, or —
// when the token carries no kid and the set holds exactly one key of the right
// type — that key. A kid that matches nothing is never silently widened into
// "try them all", which would turn key rotation into an oracle.
func (ks *keySet) candidate(kid, kty string) (signingKey, bool) {
	if kid != "" {
		for _, k := range ks.keys {
			if k.kid == kid && k.kty == kty {
				return k, true
			}
		}
		return signingKey{}, false
	}
	var found signingKey
	n := 0
	for _, k := range ks.keys {
		if k.kty == kty {
			found = k
			n++
		}
	}
	return found, n == 1
}

// jwsHeader is the protected header, restricted to what dispatch needs.
type jwsHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
	// Crit lists header parameters the verifier is required to understand
	// (RFC 7515 §4.1.11). We understand none of the extensions it could name,
	// so any "crit" at all must fail closed.
	Crit []string `json:"crit"`
}

// verify checks a compact JWS against ks and returns the raw payload. It
// performs no claim validation — that is validateClaims in oidc.go — so the
// two concerns can be tested independently.
func verify(ks *keySet, token string) ([]byte, error) {
	// SplitN(4) rather than Split: a five-part token is JWE, which this
	// package does not implement, and must not be mistaken for a JWS whose
	// trailing segments were ignored.
	parts := strings.SplitN(token, ".", 4)
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a compact JWS", ErrInvalidToken)
	}

	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header is not base64url", ErrInvalidToken)
	}
	var h jwsHeader
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return nil, fmt.Errorf("%w: header is not JSON", ErrInvalidToken)
	}
	if len(h.Crit) > 0 {
		return nil, fmt.Errorf("%w: unsupported critical header parameters", ErrInvalidToken)
	}
	if h.Typ != "" && !strings.EqualFold(h.Typ, "JWT") && !strings.EqualFold(h.Typ, "at+jwt") {
		return nil, fmt.Errorf("%w: unexpected token type %q", ErrInvalidToken, h.Typ)
	}

	av, ok := algs[h.Alg]
	if !ok {
		// This is where alg=none and alg=HS256 land: no entry, no verifier,
		// no path to a successful return.
		return nil, fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidToken, h.Alg)
	}
	key, ok := ks.candidate(h.Kid, av.kty)
	if !ok {
		return nil, fmt.Errorf("%w: no signing key for kid %q", ErrInvalidToken, h.Kid)
	}
	// A key that declares its own algorithm may only be used for that one.
	if key.alg != "" && key.alg != h.Alg {
		return nil, fmt.Errorf("%w: key %q is not for algorithm %q", ErrInvalidToken, h.Kid, h.Alg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64url", ErrInvalidToken)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !av.verify(key.pub, digest[:], sig) {
		return nil, fmt.Errorf("%w: signature does not verify", ErrInvalidToken)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload is not base64url", ErrInvalidToken)
	}
	return payload, nil
}

// b64uint decodes a base64url big-endian unsigned integer (RFC 7518 §2).
func b64uint(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("oidc: malformed key parameter")
	}
	return new(big.Int).SetBytes(b), nil
}

// b64bytes decodes a base64url field required to be exactly n bytes.
func b64bytes(s string, n int) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != n {
		return nil, errors.New("oidc: malformed key parameter")
	}
	return b, nil
}
