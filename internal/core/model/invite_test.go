package model

import (
	"encoding/base64"
	"testing"
)

func TestInviteTokenEntropyEncoding(t *testing.T) {
	seen := map[InviteTokenID]bool{}
	for range 128 {
		id := NewInviteTokenID()
		decoded, err := base64.RawURLEncoding.DecodeString(string(id))
		if err != nil || len(decoded) != 32 || len(id) != 43 {
			t.Fatalf("expected 256-bit raw URL base64 token: %q (%v)", id, err)
		}
		if seen[id] {
			t.Fatal("repeated invitation token")
		}
		seen[id] = true
	}
}
