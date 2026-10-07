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

func TestNewInviteTokenNormalizesInvitee(t *testing.T) {
	mixed, blank := " Jean.Dupont@Corp.tld\t", "  "
	if got := NewInviteToken("org", RoleMember, &mixed, nil, nil, "u").InviteeEmail(); got == nil || *got != "jean.dupont@corp.tld" {
		t.Errorf("invitee: got %v, want jean.dupont@corp.tld", got)
	}
	// A blank target names nobody: it must stay targeted, matching no one,
	// rather than widen into an open invitation anyone can use.
	if got := NewInviteToken("org", RoleMember, &blank, nil, nil, "u").InviteeEmail(); got == nil || *got != "" {
		t.Errorf("blank invitee: got %v, want a targeted invitation to \"\"", got)
	}
	if got := NewInviteToken("org", RoleMember, nil, nil, nil, "u").InviteeEmail(); got != nil {
		t.Errorf("open invitation: got %q, want nil", *got)
	}
}
