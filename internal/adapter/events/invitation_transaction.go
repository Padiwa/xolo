package events

import (
	"context"
	"strconv"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

type InvitationTransaction struct {
	backend port.InvitationTransaction
	emitter port.EventEmitter
}

func NewInvitationTransaction(backend port.InvitationTransaction, emitter port.EventEmitter) *InvitationTransaction {
	return &InvitationTransaction{backend: backend, emitter: emitter}
}

func (s *InvitationTransaction) WithInvitationTransaction(ctx context.Context, fn func(port.InvitationTx) error) error {
	var committed []model.Event
	err := s.backend.WithInvitationTransaction(ctx, func(tx port.InvitationTx) error {
		// A new buffer for every attempt, including attempts that fail at commit.
		committed = nil
		buffer := &invitationEvents{}
		if err := fn(&invitationTx{InvitationTx: tx, emitter: buffer}); err != nil {
			return err
		}
		committed = buffer.events
		return nil
	})
	if err != nil {
		return err
	}
	if s.emitter != nil {
		for _, event := range committed {
			s.emitter.Emit(ctx, event)
		}
	}
	return nil
}

type invitationEvents struct{ events []model.Event }

func (b *invitationEvents) Emit(_ context.Context, event model.Event) {
	b.events = append(b.events, event)
}

type invitationTx struct {
	port.InvitationTx
	emitter port.EventEmitter
}

func (tx *invitationTx) CreateInvite(ctx context.Context, invite model.InviteToken) error {
	if err := tx.InvitationTx.CreateInvite(ctx, invite); err != nil {
		return err
	}
	emit(ctx, tx.emitter, invite.OrgID(), model.SeverityInfo, model.EventTypeInviteCreated,
		"Invitation créée", inviteAttrs(invite))
	return nil
}

func (tx *invitationTx) DeleteInvite(ctx context.Context, id model.InviteTokenID) error {
	invite, err := tx.GetInviteByID(ctx, id)
	if err != nil {
		return err
	}
	if err := tx.InvitationTx.DeleteInvite(ctx, id); err != nil {
		return err
	}
	emit(ctx, tx.emitter, invite.OrgID(), model.SeverityWarning, model.EventTypeInviteDeleted,
		"Invitation supprimée", inviteAttrs(invite))
	return nil
}

func (tx *invitationTx) InsertInvitationMember(ctx context.Context, membership model.Membership) (bool, error) {
	created, err := tx.InvitationTx.InsertInvitationMember(ctx, membership)
	if err != nil || !created {
		return created, err
	}
	emit(ctx, tx.emitter, membership.OrgID(), model.SeverityInfo, model.EventTypeMemberAdded,
		"Membre ajouté à l'organisation", map[string]string{
			"membership_id": string(membership.ID()), "member_user_id": string(membership.UserID()),
		})
	return true, nil
}

func (tx *invitationTx) SetMembershipRoles(ctx context.Context, id model.MembershipID, roles []model.RoleID) error {
	if err := tx.InvitationTx.SetMembershipRoles(ctx, id, roles); err != nil {
		return err
	}
	membership, err := tx.GetMembership(ctx, id)
	if err != nil {
		return err
	}
	emit(ctx, tx.emitter, membership.OrgID(), model.SeverityInfo, model.EventTypeMemberUpdated,
		"Rôles de membre modifiés", map[string]string{
			"membership_id": string(id), "member_user_id": string(membership.UserID()),
			"role_count": strconv.Itoa(len(roles)),
		})
	return nil
}

var _ port.InvitationTransaction = (*InvitationTransaction)(nil)
