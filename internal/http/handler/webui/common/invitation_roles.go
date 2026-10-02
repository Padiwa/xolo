package common

import (
	"context"
	"errors"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/core/service"
)

// PrepareInvitationList rechecks scope and recipient while resolving labels.
// An invitation that is no longer visible or usable must not expose its details
// through a stale list result or a cached user's previous email address.
func PrepareInvitationList(ctx context.Context, invitations *service.InvitationService, tenantID model.TenantID, userID model.UserID, invites []model.InviteToken) ([]model.InviteToken, map[model.InviteTokenID]string, error) {
	pending := make([]model.InviteToken, 0, len(invites))
	names := make(map[model.InviteTokenID]string, len(invites))
	for _, invite := range invites {
		view, err := invitations.Prepare(ctx, tenantID, invite.ID(), userID)
		if errors.Is(err, port.ErrInvalid) || errors.Is(err, port.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		pending = append(pending, invite)
		names[invite.ID()] = view.Role.Name()
	}
	return pending, names, nil
}
