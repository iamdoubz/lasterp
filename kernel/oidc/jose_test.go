// SPDX-License-Identifier: AGPL-3.0-only

package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
)

// The RFC 7515 appendix examples are the reference implementation this package
// is checked against: complete signed tokens with their keys, published by the
// spec. If a refactor breaks compact-JWS handling, these fail before anything
// LastERP-specific does.

const (
	// RFC 7515 A.2 — RSASSA-PKCS1-v1_5 SHA-256.
	rfcRS256Token = "eyJhbGciOiJSUzI1NiJ9" +
		".eyJpc3MiOiJqb2UiLA0KICJleHAiOjEzMDA4MTkzODAsDQogImh0dHA6Ly9leGFtcGxlLmNvbS9pc19yb290Ijp0cnVlfQ" +
		".cC4hiUPoj9Eetdgtv3hF80EGrhuB__dzERat0XF9g2VtQgr9PJbu3XOiZj5RZmh7AAuHIm4Bh-0Qc_lF5YKt_O8W2Fp5juj" +
		"Gbds9uJdbF9CUAr7t1dnZcAcQjbKBYNX4BAynRFdiuB--f_nZLgrnbyTyWzO75vRK5h6xBArLIARNPvkSjtQBMHlb1L07Qe7" +
		"K0GarZRmB_eSN9383LcOLn6_dO--xi12jzDwusC-eOkHWEsqtFZESc6BfI7noOPqvhJ1phCnvWh6IeYI2w9QOYEUipUTI8np" +
		"6LbgGY9Fs98rqVt5AXLIhWkWywlVmtVrBp0igcN_IoypGlUPQGe77Rw"
	rfcRS256N = "ofgWCuLjybRlzo0tZWJjNiuSfb4p4fAkd_wWJcyQoTbji9k0l8W26mPddxHmfHQp-Vaw-4qPCJrcS2mJPMEzP1Pt0Bm4" +
		"d4QlL-yRT-SFd2lZS-pCgNMsD1W_YpRPEwOWvG6b32690r2jZ47soMZo9wGzjb_7OMg0LOL-bSf63kpaSHSXndS5z5rexMdb" +
		"BYUsLA9e-KXBdQOS-UTo7WTBEMa2R2CapHg665xsmtdVMTBQY4uDZlxvb3qCo5ZwKh9kG4LT6_I5IhlJH7aGhyxXFvUK-DWN" +
		"moudF8NAco9_h9iaGNj8q2ethFkMLs91kzk2PAcDTW9gb54h4FRWyuXpoQ"

	// RFC 7515 A.3 — ECDSA P-256 SHA-256.
	rfcES256Token = "eyJhbGciOiJFUzI1NiJ9" +
		".eyJpc3MiOiJqb2UiLA0KICJleHAiOjEzMDA4MTkzODAsDQogImh0dHA6Ly9leGFtcGxlLmNvbS9pc19yb290Ijp0cnVlfQ" +
		".DtEhU3ljbEg8L38VWAfUAqOyKAM6-Xx-F4GawxaepmXFCgfTjDxw5djxLa8ISlSApmWQxfKTUJqPP3-Kg6NU1Q"
	rfcES256X = "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU"
	rfcES256Y = "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"

	// The payload both appendix examples sign.
	rfcPayload = "{\"iss\":\"joe\",\r\n \"exp\":1300819380,\r\n \"http://example.com/is_root\":true}"
)

func TestVerifyAcceptsRFC7515Vectors(t *testing.T) {
	tests := []struct {
		name  string
		jwks  string
		token string
	}{
		{
			name:  "RS256 (RFC 7515 A.2)",
			jwks:  fmt.Sprintf(`{"keys":[{"kty":"RSA","n":%q,"e":"AQAB"}]}`, rfcRS256N),
			token: rfcRS256Token,
		},
		{
			name:  "ES256 (RFC 7515 A.3)",
			jwks:  fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","x":%q,"y":%q}]}`, rfcES256X, rfcES256Y),
			token: rfcES256Token,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ks, err := parseJWKS([]byte(tc.jwks))
			if err != nil {
				t.Fatalf("parseJWKS: %v", err)
			}
			payload, err := verify(ks, tc.token)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if string(payload) != rfcPayload {
				t.Errorf("payload = %q, want %q", payload, rfcPayload)
			}
		})
	}
}

// TestVerifyRejectsForgeries is the heart of ADR-019: every historical way of
// getting a JWS verifier to accept a token it should not. Each case must fail,
// and the RS256 vector above proves the verifier is not simply refusing
// everything.
func TestVerifyRejectsForgeries(t *testing.T) {
	signer := newTestSigner(t)
	valid := signer.sign(t, "RS256", signer.kid, `{"sub":"alice"}`)

	tests := []struct {
		name  string
		token func() string
	}{
		{
			// The signature covers the payload; changing one byte of it must
			// invalidate the token even though the header and key are right.
			name: "tampered payload",
			token: func() string {
				parts := strings.Split(valid, ".")
				parts[1] = b64(`{"sub":"admin"}`)
				return strings.Join(parts, ".")
			},
		},
		{
			// The unsecured-JWS attack: strip the signature, claim there is no
			// algorithm. There is no "none" entry in algs, so this never
			// reaches a verifier.
			name: "alg none with empty signature",
			token: func() string {
				return b64(`{"alg":"none"}`) + "." + b64(`{"sub":"admin"}`) + "."
			},
		},
		{
			// The alg-confusion attack: sign with HMAC using the issuer's
			// *public* key as the shared secret, and ask for HS256. A verifier
			// that dispatches on the token's alg without constraining the key
			// type accepts this. There is no HMAC entry in algs at all.
			name: "alg confusion, HS256 keyed with the RSA public key",
			token: func() string {
				header := b64(`{"alg":"HS256","kid":"` + signer.kid + `"}`)
				payload := b64(`{"sub":"admin"}`)
				secret := signer.rsa.N.Bytes()
				mac := hmac.New(sha256.New, secret)
				mac.Write([]byte(header + "." + payload))
				return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			},
		},
		{
			name: "unknown kid",
			token: func() string {
				return signer.sign(t, "RS256", "not-a-key-we-have", `{"sub":"alice"}`)
			},
		},
		{
			// An RSA signature presented as ES256. The kid resolves, but the
			// key's kty is not the one ES256 requires, so it is not even a
			// candidate.
			name: "algorithm/key-type mismatch",
			token: func() string {
				parts := strings.Split(valid, ".")
				parts[0] = b64(`{"alg":"ES256","kid":"` + signer.kid + `"}`)
				return strings.Join(parts, ".")
			},
		},
		{
			// RFC 7515 §4.1.11: a verifier that does not understand every
			// listed critical parameter must reject the token.
			name: "critical header parameters",
			token: func() string {
				parts := strings.Split(valid, ".")
				parts[0] = b64(`{"alg":"RS256","kid":"` + signer.kid + `","crit":["exp"],"exp":1}`)
				return strings.Join(parts, ".")
			},
		},
		{
			// Five segments is JWE. Splitting on "." and taking the first three
			// would treat an encrypted token as a signed one.
			name: "JWE, not JWS",
			token: func() string {
				return valid + ".extra"
			},
		},
		{
			name:  "empty token",
			token: func() string { return "" },
		},
		{
			name:  "header is not base64",
			token: func() string { return "!!!.eyJ9.sig" },
		},
		{
			name: "signature is truncated",
			token: func() string {
				parts := strings.Split(valid, ".")
				parts[2] = parts[2][:len(parts[2])-4]
				return strings.Join(parts, ".")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verify(signer.keySet(t), tc.token()); err == nil {
				t.Fatal("verify accepted a token it must reject")
			} else if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error = %v, want it to wrap ErrInvalidToken", err)
			}
		})
	}
}

// TestVerifyRejectsShortECDSASignature covers the fixed-width requirement in
// RFC 7518 §3.4: a signature shorter than 64 bytes must not be left-padded into
// a different (r,s) pair that happens to verify.
func TestVerifyRejectsShortECDSASignature(t *testing.T) {
	ks, err := parseJWKS([]byte(fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","x":%q,"y":%q}]}`, rfcES256X, rfcES256Y)))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	parts := strings.Split(rfcES256Token, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	// Drop the leading zero-ish byte of r and re-encode: 63 bytes.
	parts[2] = base64.RawURLEncoding.EncodeToString(sig[1:])
	if _, err := verify(ks, strings.Join(parts, ".")); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("verify with a 63-byte signature = %v, want ErrInvalidToken", err)
	}
}

// TestVerifyHonoursKeyDeclaredAlg: a JWK that declares alg RS256 must not be
// used to verify PS256, even though both are RSA over SHA-256.
func TestVerifyHonoursKeyDeclaredAlg(t *testing.T) {
	signer := newTestSigner(t)
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"alg":"RS256","n":%q,"e":"AQAB"}]}`,
		signer.kid, b64url(signer.rsa.N.Bytes()))
	ks, err := parseJWKS([]byte(jwks))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if _, err := verify(ks, signer.sign(t, "PS256", signer.kid, `{"sub":"alice"}`)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("PS256 token against an RS256-declared key = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyAcceptsPS256(t *testing.T) {
	signer := newTestSigner(t)
	if _, err := verify(signer.keySet(t), signer.sign(t, "PS256", signer.kid, `{"sub":"alice"}`)); err != nil {
		t.Fatalf("verify PS256: %v", err)
	}
}

// TestVerifyWithoutKidNeedsAnUnambiguousKey: a token with no kid is only
// verifiable when the set holds exactly one key of the right type. Two
// candidates must not be tried in turn.
func TestVerifyWithoutKidNeedsAnUnambiguousKey(t *testing.T) {
	one := newTestSigner(t)
	two := newTestSigner(t)
	token := one.sign(t, "RS256", "", `{"sub":"alice"}`)

	if _, err := verify(one.keySet(t), token); err != nil {
		t.Fatalf("single-key set: %v", err)
	}

	both := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"n":%q,"e":"AQAB"},{"kty":"RSA","kid":%q,"n":%q,"e":"AQAB"}]}`,
		one.kid, b64url(one.rsa.N.Bytes()), two.kid, b64url(two.rsa.N.Bytes()))
	ks, err := parseJWKS([]byte(both))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if _, err := verify(ks, token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("kid-less token against a two-key set = %v, want ErrInvalidToken", err)
	}
}

func TestParseJWKS(t *testing.T) {
	signer := newTestSigner(t)
	good := fmt.Sprintf(`{"kty":"RSA","kid":%q,"n":%q,"e":"AQAB"}`, signer.kid, b64url(signer.rsa.N.Bytes()))

	tests := []struct {
		name    string
		jwks    string
		wantErr bool
		wantN   int
	}{
		{name: "empty document", jwks: `{"keys":[]}`, wantErr: true},
		{name: "not JSON", jwks: `{`, wantErr: true},
		{name: "only unusable keys", jwks: `{"keys":[{"kty":"oct","k":"AAAA"}]}`, wantErr: true},
		{
			// An IdP publishing an encryption key alongside its signing key is
			// normal; the encryption key must be skipped, not fatal, and must
			// never verify anything.
			name:  "encryption key is skipped, signing key kept",
			jwks:  fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"enc","kid":"e","n":%q,"e":"AQAB"},%s]}`, b64url(signer.rsa.N.Bytes()), good),
			wantN: 1,
		},
		{
			name:  "unsupported key type is skipped",
			jwks:  fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA"},%s]}`, good),
			wantN: 1,
		},
		{
			// A 1024-bit modulus is not something we let into the trust set,
			// however cheerfully the IdP publishes it.
			name:    "undersized RSA key",
			jwks:    `{"keys":[{"kty":"RSA","kid":"weak","n":"sMHT-cGmpNqCEIrTOo6oCQMj_UPQBLcxvFYUcSb1eV8HpFONS5xTn5MGm5MUZDJKgIYRLcKMKfvfj-J1JhIcQGY_9OcnEs87yRvCkMKSaCw_LZ_37ZKPSKLwCIKmB6BXOlvBWlqCFOKJUZ-CVoKKHpVdbGSU2yDbTLM8kzUZfN0","e":"AQAB"}]}`,
			wantErr: true,
		},
		{
			name:    "EC point not on the curve",
			jwks:    fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","x":%q,"y":%q}]}`, rfcES256X, rfcES256X),
			wantErr: true,
		},
		{
			name:    "unsupported curve",
			jwks:    fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-521","x":%q,"y":%q}]}`, rfcES256X, rfcES256Y),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ks, err := parseJWKS([]byte(tc.jwks))
			if tc.wantErr {
				if err == nil {
					t.Fatal("parseJWKS succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJWKS: %v", err)
			}
			if len(ks.keys) != tc.wantN {
				t.Errorf("usable keys = %d, want %d", len(ks.keys), tc.wantN)
			}
		})
	}
}

// FuzzParseJWKS: the JWKS is remote input parsed before any signature is
// checked, so it must not panic on anything an IdP (or something pretending to
// be one) can serve.
func FuzzParseJWKS(f *testing.F) {
	f.Add(`{"keys":[]}`)
	f.Add(fmt.Sprintf(`{"keys":[{"kty":"RSA","n":%q,"e":"AQAB"}]}`, rfcRS256N))
	f.Add(fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","x":%q,"y":%q}]}`, rfcES256X, rfcES256Y))
	f.Add(`{"keys":[{"kty":"RSA","n":"","e":""}]}`)
	f.Add(`{"keys":null}`)
	f.Fuzz(func(t *testing.T, doc string) {
		_, _ = parseJWKS([]byte(doc))
	})
}

// FuzzVerify: likewise for the token itself, which is attacker-controlled by
// definition. Any input that verifies against a key the fuzzer does not have
// would be a finding; in practice this asserts no panic.
func FuzzVerify(f *testing.F) {
	f.Add(rfcRS256Token)
	f.Add(rfcES256Token)
	f.Add("a.b.c")
	f.Add("")
	ks, err := parseJWKS([]byte(fmt.Sprintf(`{"keys":[{"kty":"RSA","n":%q,"e":"AQAB"}]}`, rfcRS256N)))
	if err != nil {
		f.Fatalf("parseJWKS: %v", err)
	}
	f.Fuzz(func(t *testing.T, token string) {
		payload, err := verify(ks, token)
		if err == nil && token != rfcRS256Token {
			t.Fatalf("verify accepted an unexpected token %q with payload %q", token, payload)
		}
	})
}

// testSigner is a throwaway RSA signing identity standing in for an IdP.
type testSigner struct {
	kid string
	rsa *rsa.PrivateKey
}

// signerSeq keeps every test signer's kid distinct, which the two-key
// ambiguity test depends on.
var signerSeq atomic.Int64

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	// 2048 is the smallest modulus parseJWKS accepts and the slowest part of
	// these tests; anything larger buys nothing here.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return &testSigner{kid: fmt.Sprintf("test-key-%d", signerSeq.Add(1)), rsa: key}
}

func (s *testSigner) keySet(t *testing.T) *keySet {
	t.Helper()
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"n":%q,"e":"AQAB"}]}`,
		s.kid, b64url(s.rsa.N.Bytes()))
	ks, err := parseJWKS([]byte(jwks))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	return ks
}

func (s *testSigner) sign(t *testing.T, alg, kid, payload string) string {
	t.Helper()
	header := map[string]string{"alg": alg}
	if kid != "" {
		header["kid"] = kid
	}
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	signingInput := b64url(h) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signingInput))

	var sig []byte
	switch alg {
	case "RS256":
		sig, err = rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, digest[:])
	case "PS256":
		sig, err = rsa.SignPSS(rand.Reader, s.rsa, crypto.SHA256, digest[:], &rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256,
		})
	default:
		t.Fatalf("testSigner cannot sign %s", alg)
	}
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64url(sig)
}

// signES256 produces the fixed-width r||s form RFC 7518 §3.4 requires.
func signES256(t *testing.T, key *ecdsa.PrivateKey, signingInput string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("ecdsa sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return b64url(sig)
}

func TestVerifyAcceptsGeneratedES256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"EC","kid":"ec","crv":"P-256","x":%q,"y":%q}]}`,
		b64url(bigTo32(key.X)), b64url(bigTo32(key.Y)))
	ks, err := parseJWKS([]byte(jwks))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	signingInput := b64(`{"alg":"ES256","kid":"ec"}`) + "." + b64(`{"sub":"alice"}`)
	if _, err := verify(ks, signingInput+"."+signES256(t, key, signingInput)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func bigTo32(i *big.Int) []byte {
	b := make([]byte, 32)
	i.FillBytes(b)
	return b
}

func b64(s string) string    { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
