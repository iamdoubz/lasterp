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
