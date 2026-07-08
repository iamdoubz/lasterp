// SPDX-License-Identifier: AGPL-3.0-only

// RFC 6238 TOTP over stdlib crypto/hmac + crypto/sha1 — no dependency
// needed (docs/notes/WP-0.3-decisions.md).
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const totpStep = 30 * time.Second

var base32Enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a fresh random base32 secret.
func GenerateTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32Enc.EncodeToString(b), nil
}

func totpCounter(secret string, at time.Time, stepOffset int64) (string, int64, error) {
	key, err := base32Enc.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", 0, fmt.Errorf("identity: decode TOTP secret: %w", err)
	}
	counter := at.Unix()/int64(totpStep.Seconds()) + stepOffset

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", truncated%1_000_000), counter, nil
}

// ValidateTOTP checks code against secret, tolerating one 30s step of
// clock skew in either direction. lastCounter, if present, rejects a
// replayed code from an already-consumed step. On success it returns the
// counter that matched, for the caller to persist as the new lastCounter.
func ValidateTOTP(secret, code string, at time.Time, lastCounter *int64) (bool, int64, error) {
	for _, stepOffset := range []int64{0, -1, 1} {
		want, counter, err := totpCounter(secret, at, stepOffset)
		if err != nil {
			return false, 0, err
		}
		if want != code {
			continue
		}
		if lastCounter != nil && counter <= *lastCounter {
			return false, 0, nil // replay of an already-consumed step
		}
		return true, counter, nil
	}
	return false, 0, nil
}
