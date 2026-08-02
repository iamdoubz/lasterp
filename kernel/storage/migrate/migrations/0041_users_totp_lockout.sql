-- WP-1.12: per-user second-factor lockout (decisions §6).
--
-- The gateway's rate limit is 100 req/s keyed by client IP. ValidateTOTP
-- tolerates a ±1 step window, so three of the 10^6 codes are live at any
-- instant: an attacker who already holds the password expects a hit in under
-- an hour from a single IP. A second factor with that property is decorative.
--
-- The counter only advances after the password has already verified, so it is
-- not a lever for an anonymous attacker to lock accounts they cannot otherwise
-- touch.
ALTER TABLE users ADD COLUMN totp_failed_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_locked_until TIMESTAMPTZ;
