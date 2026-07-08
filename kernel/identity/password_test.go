package identity

import "testing"

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("hash equals plaintext")
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("VerifyPassword accepted the wrong password")
	}
}
