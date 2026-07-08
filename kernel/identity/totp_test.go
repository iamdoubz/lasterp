package identity

import (
	"testing"
	"time"
)

func TestTOTPValidCodeAccepted(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	now := time.Now().UTC()
	code, _, err := totpCounter(secret, now, 0)
	if err != nil {
		t.Fatalf("totpCounter: %v", err)
	}

	ok, _, err := ValidateTOTP(secret, code, now, nil)
	if err != nil {
		t.Fatalf("ValidateTOTP: %v", err)
	}
	if !ok {
		t.Fatal("valid code was rejected")
	}
}

func TestTOTPInvalidCodeRejected(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	ok, _, err := ValidateTOTP(secret, "000000", time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("ValidateTOTP: %v", err)
	}
	if ok {
		t.Fatal("arbitrary code was accepted")
	}
}

func TestTOTPClockSkewTolerated(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	now := time.Now().UTC()

	// Code generated for the step before "now" (up to 30s of skew) must
	// still validate.
	code, _, err := totpCounter(secret, now, -1)
	if err != nil {
		t.Fatalf("totpCounter: %v", err)
	}
	ok, _, err := ValidateTOTP(secret, code, now, nil)
	if err != nil {
		t.Fatalf("ValidateTOTP: %v", err)
	}
	if !ok {
		t.Fatal("code within tolerated clock skew was rejected")
	}
}

func TestTOTPReplayRejected(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	now := time.Now().UTC()
	code, counter, err := totpCounter(secret, now, 0)
	if err != nil {
		t.Fatalf("totpCounter: %v", err)
	}

	ok, gotCounter, err := ValidateTOTP(secret, code, now, nil)
	if err != nil || !ok {
		t.Fatalf("first use: ok=%v err=%v", ok, err)
	}
	if gotCounter != counter {
		t.Fatalf("counter = %d, want %d", gotCounter, counter)
	}

	// Same code, same step, presented again: must be rejected once the
	// counter is recorded as last-used.
	replayed, _, err := ValidateTOTP(secret, code, now, &gotCounter)
	if err != nil {
		t.Fatalf("ValidateTOTP (replay): %v", err)
	}
	if replayed {
		t.Fatal("replayed code within the same step was accepted")
	}
}
