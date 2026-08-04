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

// IsV7 reports whether s is a UUIDv7 in the canonical string form New
// returns.
//
// Canonical is required, not merely parseable: uuid.Parse also accepts the
// URN, braced and unhyphenated forms, and accepting one of those would mean
// storing an id in a different spelling from the one the caller sent. A
// client that applied a row optimistically under its own id (WP-2.3-
// decisions.md §2) would then hold a row the server does not have, which is
// the divergence the client-supplied id exists to prevent.
func IsV7(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.Version() == 7 && id.String() == s
}
