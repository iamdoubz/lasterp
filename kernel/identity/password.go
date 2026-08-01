// SPDX-License-Identifier: AGPL-3.0-only

package identity

import "golang.org/x/crypto/bcrypt"

// HashPassword returns a bcrypt hash suitable for storage in
// users.password_hash. Never store or log the plaintext.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyPassword reports whether plain matches hash.
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// dummyHash is a real bcrypt hash of a value nobody logs in with. Verifying
// against it makes the unknown-user path cost the same as the wrong-password
// path: bcrypt dominates the wall clock of a login, so returning early for an
// address that does not exist is a timing oracle for "this email is a user
// here" — and email addresses are exactly what an attacker enumerates first.
//
// Generated at init rather than hardcoded so it tracks whatever cost
// HashPassword uses. An empty string (if bcrypt somehow failed) is harmless:
// CompareHashAndPassword rejects it after doing the same work.
var dummyHash = func() string {
	h, err := HashPassword("lasterp-timing-equalizer")
	if err != nil {
		return ""
	}
	return h
}()

// EqualizeVerifyTiming burns the same work a real password check would, for
// callers that have already decided the answer is no. It lives here rather than
// at each call site because a caller who has to remember to add it is a caller
// who will forget — which is exactly what happened before WP-1.10: the HTTP
// login handler had it and PasswordTOTPProvider, the supposedly reusable
// implementation, did not.
func EqualizeVerifyTiming(plain string) {
	_ = VerifyPassword(dummyHash, plain)
}
