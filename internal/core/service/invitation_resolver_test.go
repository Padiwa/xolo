package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

type resolverInviteReader struct {
	invite model.InviteToken
	err    error
}

func (s resolverInviteReader) GetInviteByID(ctx context.Context, id model.InviteTokenID) (model.InviteToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.invite == nil || s.invite.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.invite, nil
}

type resolverOrgReader struct {
	org   model.Organization
	err   error
	calls int
}

func (s *resolverOrgReader) GetOrgByID(ctx context.Context, id model.OrgID) (model.Organization, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.org == nil || s.org.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.org, nil
}

type revokedResolverInvite struct {
	model.InviteToken
	revoked time.Time
}

func (i revokedResolverInvite) RevokedAt() *time.Time { return &i.revoked }
func (i revokedResolverInvite) UsesCount() int        { return 1 }

func TestInvitationResolver(t *testing.T) {
	org := model.NewOrganization("tenant", "acme", "Acme", "")
	invite := model.NewInviteToken(org.ID(), model.RoleMember, nil, nil, nil, "creator")
	past := time.Now().Add(-time.Hour)
	maxUses := 1
	expired := model.NewInviteToken(org.ID(), model.RoleMember, nil, &past, &maxUses, "creator")
	backendErr := errors.New("backend unavailable")
	cases := []struct {
		name                       string
		invite                     model.InviteToken
		org                        model.Organization
		tenantID                   model.TenantID
		orgID                      model.OrgID
		inviteErr, orgErr, wantErr error
	}{
		{name: "valid", invite: invite, org: org, tenantID: org.TenantID(), orgID: org.ID()},
		{name: "foreign organization", invite: invite, org: org, tenantID: org.TenantID(), orgID: "other", wantErr: port.ErrNotFound},
		{name: "foreign tenant", invite: invite, org: org, tenantID: "other", orgID: org.ID(), wantErr: port.ErrNotFound},
		{name: "missing invitation", org: org, tenantID: org.TenantID(), orgID: org.ID(), wantErr: port.ErrNotFound},
		{name: "missing parent", invite: invite, tenantID: org.TenantID(), orgID: org.ID(), wantErr: port.ErrNotFound},
		{name: "invitation error", org: org, inviteErr: fmt.Errorf("read: %w", backendErr), wantErr: backendErr},
		{name: "parent error", invite: invite, orgErr: fmt.Errorf("read: %w", backendErr), wantErr: backendErr},
		{name: "expired", invite: expired, org: org, tenantID: org.TenantID(), orgID: org.ID()},
		{name: "revoked and exhausted", invite: revokedResolverInvite{expired, past}, org: org, tenantID: org.TenantID(), orgID: org.ID()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, inOrg := range []bool{false, true} {
				t.Run(fmt.Sprintf("inOrg=%v", inOrg), func(t *testing.T) {
					orgs := &resolverOrgReader{org: tc.org, err: tc.orgErr}
					resolver := NewInvitationResolver(resolverInviteReader{tc.invite, tc.inviteErr}, orgs)
					id := invite.ID()
					if tc.invite != nil {
						id = tc.invite.ID()
					}
					var got model.InviteToken
					var parent model.Organization
					var err error
					if inOrg {
						got, parent, err = resolver.ResolveInOrg(t.Context(), tc.tenantID, tc.orgID, id)
					} else {
						got, parent, err = resolver.Resolve(t.Context(), tc.tenantID, id)
					}
					wantErr := tc.wantErr
					if tc.name == "foreign organization" && !inOrg {
						wantErr = nil
					}
					if !errors.Is(err, wantErr) {
						t.Fatalf("error = %v, want %v", err, wantErr)
					}
					if wantErr != nil {
						if got != nil || parent != nil {
							t.Fatal("failed resolution leaked objects")
						}
					} else if got != tc.invite || parent != tc.org {
						t.Fatal("resolution returned different objects")
					}
					if (tc.invite == nil || tc.inviteErr != nil) && orgs.calls != 0 {
						t.Fatal("parent read after invitation lookup failed")
					}
				})
			}
		})
	}
	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		resolver := NewInvitationResolver(resolverInviteReader{invite: invite}, &resolverOrgReader{org: org})
		if _, _, err := resolver.Resolve(ctx, org.TenantID(), invite.ID()); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}
