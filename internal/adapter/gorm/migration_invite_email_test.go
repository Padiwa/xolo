package gorm_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	"github.com/xolo-gateway/xolo/internal/core/model"
	gormpkg "gorm.io/gorm"
)

// TestUpgradeNormalizesInviteeEmails covers 202610070002 on an instance that
// already holds invitations written before addresses were normalized. A fresh
// install goes through InitSchema, which records the migration without running it.
func TestUpgradeNormalizesInviteeEmails(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		store := xologorm.NewStore(db)
		require.NoError(t, store.Migrate(t.Context()))
		tenant := model.NewTenant("acme", "Acme", "")
		require.NoError(t, store.CreateTenant(t.Context(), tenant))
		org := model.NewOrganization(tenant.ID(), "acme", "Acme", "")
		require.NoError(t, store.CreateOrg(t.Context(), org))

		// Reconstruct the rows an earlier binary wrote, bypassing NewInviteToken.
		// The non-ASCII capital and the tab are what SQLite's LOWER() and TRIM()
		// would leave alone: the stored form must equal model.NormalizeEmail.
		want := map[string]*string{
			" Jean.Dupont@Corp.tld ": ptr("jean.dupont@corp.tld"),
			"\tÉlodie@Corp.tld":      ptr("élodie@corp.tld"),
			// A blank target matches nobody; it must not become an open link.
			"  ": ptr(""),
		}
		ids := map[string]model.InviteTokenID{}
		for raw := range want {
			invite := model.NewInviteToken(org.ID(), model.RoleMember, ptr("placeholder@corp.tld"), nil, nil, model.NewUserID())
			require.NoError(t, store.CreateInvite(t.Context(), invite))
			require.NoError(t, db.Model(&xologorm.InviteToken{}).Where("id = ?", string(invite.ID())).Update("invitee_email", raw).Error)
			ids[raw] = invite.ID()
		}
		open := model.NewInviteToken(org.ID(), model.RoleMember, nil, nil, nil, model.NewUserID())
		require.NoError(t, store.CreateInvite(t.Context(), open))
		require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "202610070002").Error)

		require.NoError(t, xologorm.NewStore(db).Migrate(t.Context()))

		for raw, expected := range want {
			invite, err := store.GetInviteByID(t.Context(), ids[raw])
			require.NoError(t, err)
			require.Equal(t, expected, invite.InviteeEmail(), "stored %q", raw)
		}
		invite, err := store.GetInviteByID(t.Context(), open.ID())
		require.NoError(t, err)
		require.Nil(t, invite.InviteeEmail())
	})
}
