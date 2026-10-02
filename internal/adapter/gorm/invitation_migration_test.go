package gorm_test

import (
	"testing"
	"time"

	"github.com/rs/xid"
	"github.com/stretchr/testify/require"
	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	gormpkg "gorm.io/gorm"
)

func prepareInvitationUpgrade(t *testing.T, db *gormpkg.DB) {
	t.Helper()
	require.NoError(t, db.Exec("DROP INDEX idx_memberships_user_org").Error)
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id IN (?, ?)", "202609300001", "202609300002").Error)
}

func TestInvitationMigrationFreshAndUpgrade(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		store := newStoreOn(t, db)
		f := newInvitationFixture(t, store)
		require.True(t, db.Migrator().HasIndex(&xologorm.Membership{}, "idx_memberships_user_org"))
		m := model.NewMembership(f.user.ID(), f.org.ID())
		require.NoError(t, store.AddMember(f.ctx, m))
		require.NoError(t, store.SetMembershipRoles(f.ctx, m.ID(), []model.RoleID{f.role.ID()}))
		require.Error(t, store.AddMember(f.ctx, model.NewMembership(f.user.ID(), f.org.ID())))
		prepareInvitationUpgrade(t, db)
		past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
		old := &xologorm.InviteToken{ID: xid.New().String(), OrgID: string(f.org.ID()), Role: string(f.role.ID()), CreatedByUserID: string(f.user.ID())}
		futureOffset := time.Now().Add(time.Hour).In(time.FixedZone("west", -12*60*60))
		old.ExpiresAt = &futureOffset
		oldRevoked := *old
		oldRevoked.ID = xid.New().String()
		oldRevoked.RevokedAt = &past
		oldExpired := *old
		oldExpired.ID = xid.New().String()
		pastOffset := time.Now().Add(-time.Hour).In(time.FixedZone("east", 14*60*60))
		oldExpired.ExpiresAt = &pastOffset
		oldExhausted := *old
		oldExhausted.ID = xid.New().String()
		oldExhausted.MaxUses = ptr(1)
		oldExhausted.UsesCount = 1
		for _, inv := range []*xologorm.InviteToken{old, &oldRevoked, &oldExpired, &oldExhausted} {
			require.NoError(t, db.Create(inv).Error)
		}
		secure := f.invite(t, false, "", nil, nil)
		require.NoError(t, xologorm.NewStore(db).Migrate(f.ctx))
		revoked, err := store.GetInviteByID(f.ctx, model.InviteTokenID(old.ID))
		require.NoError(t, err)
		require.NotNil(t, revoked.RevokedAt())
		_, err = f.service.Accept(f.ctx, f.tenant.ID(), revoked.ID(), f.other.ID())
		require.ErrorIs(t, err, port.ErrInvalid)
		inv, err := store.GetInviteByID(f.ctx, secure.ID())
		require.NoError(t, err)
		require.Nil(t, inv.RevokedAt())
		inv, err = store.GetInviteByID(f.ctx, model.InviteTokenID(oldRevoked.ID))
		require.NoError(t, err)
		require.True(t, inv.RevokedAt().Equal(past))
		for _, id := range []string{oldExpired.ID, oldExhausted.ID} {
			inv, err = store.GetInviteByID(f.ctx, model.InviteTokenID(id))
			require.NoError(t, err)
			require.Nil(t, inv.RevokedAt())
		}
		require.True(t, db.Migrator().HasIndex(&xologorm.Membership{}, "idx_memberships_user_org"))
		loaded, err := store.GetMembership(f.ctx, m.ID())
		require.NoError(t, err)
		require.Len(t, loaded.Roles(), 1)
		require.Equal(t, f.role.ID(), loaded.Roles()[0].ID())
		// A normal restart and explicitly replayed migrations preserve the revocation timestamp.
		require.NoError(t, xologorm.NewStore(db).Migrate(f.ctx))
		require.NoError(t, db.Exec("DELETE FROM migrations WHERE id IN (?, ?)", "202609300001", "202609300002").Error)
		require.NoError(t, xologorm.NewStore(db).Migrate(f.ctx))
		again, err := store.GetInviteByID(f.ctx, revoked.ID())
		require.NoError(t, err)
		require.True(t, again.RevokedAt().Equal(*revoked.RevokedAt()))
		// Revoked links remain listable and deletable by administrative stores.
		listed, err := store.ListInvites(f.ctx, f.org.ID())
		require.NoError(t, err)
		require.Len(t, listed, 5)
		require.NoError(t, store.DeleteInvite(f.ctx, revoked.ID()))
	})
}

func TestInvitationMigrationDuplicateDiagnostic(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		store := newStoreOn(t, db)
		f := newInvitationFixture(t, store)
		prepareInvitationUpgrade(t, db)
		a, b := model.NewMembership(f.user.ID(), f.org.ID()), model.NewMembership(f.user.ID(), f.org.ID())
		require.NoError(t, store.AddMember(f.ctx, a))
		require.NoError(t, store.AddMember(f.ctx, b))
		require.NoError(t, store.SetMembershipRoles(f.ctx, a.ID(), []model.RoleID{f.role.ID()}))
		roles, err := store.ListOrgRoles(f.ctx, f.org.ID())
		require.NoError(t, err)
		require.NoError(t, store.SetMembershipRoles(f.ctx, b.ID(), []model.RoleID{roles[0].ID()}))
		beforeA, err := store.GetMembership(f.ctx, a.ID())
		require.NoError(t, err)
		beforeB, err := store.GetMembership(f.ctx, b.ID())
		require.NoError(t, err)
		err = xologorm.NewStore(db).Migrate(f.ctx)
		require.ErrorContains(t, err, "duplicate memberships")
		require.ErrorContains(t, err, string(f.user.ID()))
		require.ErrorContains(t, err, string(f.org.ID()))
		afterA, err := store.GetMembership(f.ctx, a.ID())
		require.NoError(t, err)
		require.Equal(t, beforeA, afterA)
		afterB, err := store.GetMembership(f.ctx, b.ID())
		require.NoError(t, err)
		require.Equal(t, beforeB, afterB)
		require.False(t, db.Migrator().HasIndex(&xologorm.Membership{}, "idx_memberships_user_org"))
		var applied int64
		require.NoError(t, db.Table("migrations").Where("id IN (?, ?)", "202609300001", "202609300002").Count(&applied).Error)
		require.Zero(t, applied)
	})
}
