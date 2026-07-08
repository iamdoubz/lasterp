// SPDX-License-Identifier: AGPL-3.0-only

// Package idgen mints primary-key IDs as UUIDv7 (docs/03-DATA-MODEL.md:
// "PKs: UUIDv7") — time-ordered, so IDs sort chronologically and index
// locality stays good under high insert rates, unlike random UUIDv4.
package idgen

import "github.com/google/uuid"

// New returns a new UUIDv7 string. Panics only if the system CSPRNG is
// unavailable (crypto/rand read failure) — never expected in practice, the
// same failure mode session token generation elsewhere already assumes
// won't happen.
func New() string {
	return uuid.Must(uuid.NewV7()).String()
}
