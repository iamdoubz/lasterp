// SPDX-License-Identifier: AGPL-3.0-only

// RFC 6238 TOTP over stdlib crypto/hmac + crypto/sha1 — no dependency
// needed (docs/notes/WP-0.3-decisions.md).
package identity

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	// #nosec G505 -- RFC 6238 defines TOTP over HMAC-SHA1 and every
	// authenticator app implements that. Using a stronger hash here would be
	// interoperable with nothing.
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
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

// TOTPURI builds the otpauth:// URI an authenticator app scans.
//
// The issuer is "LastERP (<tenant id>)" rather than a bare "LastERP", because a
// user with accounts in two tenants would otherwise get two indistinguishable
// entries in their authenticator. The tenant *id* is used, not a display name:
// it is what the user already types on the login screen, and it does not change
// under them when someone renames the tenant.
//
// algorithm/digits/period are written explicitly even though they are the
// defaults — some apps honour them, and being explicit stops one from
// defaulting to SHA-256 against this SHA-1 implementation.
func TOTPURI(tenant tenancy.ID, email, secret string) string {
	issuer := "LastERP (" + string(tenant) + ")"
	q := url.Values{
		"secret":    {secret},
		"issuer":    {issuer},
		"algorithm": {"SHA1"},
		"digits":    {"6"},
		"period":    {fmt.Sprintf("%d", int(totpStep.Seconds()))},
	}
	// The label is issuer:account, each path-escaped: an email is allowed to
	// contain characters that would otherwise end the path segment.
	label := url.PathEscape(issuer) + ":" + url.PathEscape(email)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// TOTPQRDataURI renders uri as a PNG data URI for an <img> tag.
//
// Server-side rather than client-side because every capability must be
// reachable via API/MCP: a CLI or MCP client enrolling a user has to be able to
// display a QR without reimplementing an encoder. PNG rather than SVG because
// the matrix is already a bitmap and it keeps "is a data: SVG a script vector"
// out of every future security review. In the JSON body rather than behind a
// second GET because a route serving the image would be a cacheable GET
// returning a credential (ADR-020, decisions §3).
func TOTPQRDataURI(uri string) (string, error) {
	// Level M (~15%) is what authenticator documentation assumes, with enough
	// margin for a screen photographed at an angle.
	code, err := qr.Encode(uri, qr.M)
	if err != nil {
		return "", fmt.Errorf("identity: encode TOTP QR: %w", err)
	}
	var b bytes.Buffer
	b.WriteString("data:image/png;base64,")
	enc := base64.NewEncoder(base64.StdEncoding, &b)
	if _, err := enc.Write(code.PNG()); err != nil {
		return "", fmt.Errorf("identity: encode TOTP QR: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("identity: encode TOTP QR: %w", err)
	}
	return b.String(), nil
}

// CurrentTOTPCode returns the code valid for the step containing at.
//
// It exists so callers outside this package can act as an authenticator
// without a second RFC 6238 implementation — today that is the integrity suite,
// which drives the enrollment flow over HTTP and has to produce the code a
// phone would. Generating one is not a privileged operation: it needs the
// secret, and anyone holding that is already the second factor.
func CurrentTOTPCode(secret string, at time.Time) (string, error) {
	code, _, err := totpCounter(secret, at, 0)
	return code, err
}

func totpCounter(secret string, at time.Time, stepOffset int64) (string, int64, error) {
	key, err := base32Enc.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", 0, fmt.Errorf("identity: decode TOTP secret: %w", err)
	}
	counter := at.Unix()/int64(totpStep.Seconds()) + stepOffset

	var buf [8]byte
	// #nosec G115 -- RFC 6238 encodes the step counter as a 64-bit big-endian
	// value; reinterpreting the int64 bit pattern is exactly that encoding,
	// and the counter is non-negative for any clock after 1970.
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
