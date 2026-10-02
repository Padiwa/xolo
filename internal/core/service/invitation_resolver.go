package service

import (
	"context"
	"fmt"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

type invitationReader interface {
	GetInviteByID(context.Context, model.InviteTokenID) (model.InviteToken, error)
}

type invitationOrgReader interface {
	GetOrgByID(context.Context, model.OrgID) (model.Organization, error)
}

// InvitationResolver checks an invitation's parent scope without granting any
// permission or checking whether it can be consumed. Expired, revoked and used
// invitations must remain available for administration.
//
// Readers may be bound to a transaction. Consumers accepting an invitation must
// resolve and validate it again inside the transaction that consumes it; an
// earlier resolution provides no atomicity guarantee.
type InvitationResolver struct {
	invites invitationReader
	orgs    invitationOrgReader
}

func NewInvitationResolver(invites invitationReader, orgs invitationOrgReader) *InvitationResolver {
	return &InvitationResolver{invites: invites, orgs: orgs}
}

// Resolve returns the invitation and its organization only within tenantID.
// A missing or foreign parent is indistinguishable from a missing invitation.
func (r *InvitationResolver) Resolve(ctx context.Context, tenantID model.TenantID, inviteID model.InviteTokenID) (model.InviteToken, model.Organization, error) {
	invite, err := r.invites.GetInviteByID(ctx, inviteID)
	if err != nil {
		return nil, nil, fmt.Errorf("get invitation: %w", err)
	}
	org, err := r.orgs.GetOrgByID(ctx, invite.OrgID())
	if err != nil {
		return nil, nil, fmt.Errorf("get invitation organization: %w", err)
	}
	if org.TenantID() != tenantID {
		return nil, nil, port.ErrNotFound
	}
	return invite, org, nil
}

// ResolveInOrg additionally checks the organization expected by the caller.
func (r *InvitationResolver) ResolveInOrg(ctx context.Context, tenantID model.TenantID, orgID model.OrgID, inviteID model.InviteTokenID) (model.InviteToken, model.Organization, error) {
	invite, org, err := r.Resolve(ctx, tenantID, inviteID)
	if err != nil {
		return nil, nil, err
	}
	if org.ID() != orgID {
		return nil, nil, port.ErrNotFound
	}
	return invite, org, nil
}
