// SPDX-License-Identifier: AGPL-3.0-only

// UUIDv7 for the client.
//
// The platform gives us `crypto.randomUUID()`, which is v4, and v4 is the wrong
// answer twice over. A row id is a UUIDv7 because ids sort chronologically and
// index locality matters under insert load (docs/03) — the server enforces that
// (`kernel/idgen.IsV7`) and refuses anything else with a 422, which is how this
// function came to be written rather than assumed. A command_id is a UUIDv7
// because docs/04 §Upstream 1 says so, and because it doubles as the
// Idempotency-Key.
//
// Hand-rolled rather than a dependency: it is fifteen lines over
// `crypto.getRandomValues`, and a new runtime dependency needs an ADR
// (CLAUDE.md).

/** newId returns a UUIDv7 in the canonical form the server accepts.
 *
 * Layout (RFC 9562 §5.7): 48-bit big-endian Unix milliseconds, 4-bit version,
 * 12 random bits, 2-bit variant, 62 random bits. */
export function newId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  const ms = Date.now();

  // 48-bit timestamp, most significant byte first. The high two bytes come off
  // the top of a number that exceeds 32 bits, so they are divided out rather
  // than shifted — bitwise operators in JavaScript truncate to 32 bits, and
  // `ms >>> 40` would silently be zero until the year 10889.
  bytes[0] = Math.floor(ms / 2 ** 40) & 0xff;
  bytes[1] = Math.floor(ms / 2 ** 32) & 0xff;
  bytes[2] = (ms >>> 24) & 0xff;
  bytes[3] = (ms >>> 16) & 0xff;
  bytes[4] = (ms >>> 8) & 0xff;
  bytes[5] = ms & 0xff;

  bytes[6] = (bytes[6] & 0x0f) | 0x70; // version 7
  bytes[8] = (bytes[8] & 0x3f) | 0x80; // RFC 9562 variant

  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
