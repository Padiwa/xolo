package gorm_test

import (
	"context"
	"testing"

	"github.com/rs/xid"
	"github.com/stretchr/testify/require"
	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/rbac"
	gormpkg "gorm.io/gorm"
)

// A team created locally before the common contract carries an xid: the
// migration gives it a UUID, keeps everything bound to it, and publishes the
// move.
func TestMigrateTeamContractConvertsLocalTeams(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		ctx := context.Background()
		store := newSeededStore(t, db)
		f := newTeamFixture(t, store)
		legacy := model.TeamID(xid.New().String())
		team := model.NewTeam(f.org.ID(), "legal", "Juridique", "")
		team.SetID(legacy)
		team.SetModelGrants([]model.ModelGrant{{ModelID: "m1", Kind: rbac.ModelKindLLM}})
		require.NoError(t, store.CreateTeam(ctx, team))
		require.NoError(t, store.SetTeamMember(ctx, legacy, f.membership.ID(), []model.RoleID{f.role.ID()}))
		app := model.NewApplication(f.org.ID(), "Batch", "", true)
		app.SetTeamID(legacy)
		require.NoError(t, store.CreateApplication(ctx, app))
		require.NoError(t, store.SetQuota(ctx, model.NewQuota(model.QuotaScopeTeam, string(legacy), "EUR", nil, ptrTo(int64(100)), nil)))
		require.NoError(t, store.SetQuota(ctx, model.NewQuota(model.QuotaScopeTeamMember, model.TeamMemberScopeID(legacy, f.user.ID()), "EUR", nil, ptrTo(int64(10)), nil)))

		cursor, err := store.CaptureEventCursor(ctx)
		require.NoError(t, err)
		require.NoError(t, db.Transaction(func(tx *gormpkg.DB) error { return xologorm.MigrateTeamContract(tx) }))

		teams, err := store.ListOrgTeams(ctx, f.org.ID())
		require.NoError(t, err)
		require.Len(t, teams, 1)
		migrated := teams[0]
		_, err = model.ParseTeamID(string(migrated.ID()))
		require.NoError(t, err, "the team gets a UUID")
		require.Equal(t, "legal", migrated.Slug())
		require.Equal(t, model.StatusActive, migrated.Status())
		require.Equal(t, []model.ModelGrant{{ModelID: "m1", Kind: rbac.ModelKindLLM}}, migrated.ModelGrants())
		members, err := store.ListTeamMembers(ctx, migrated.ID())
		require.NoError(t, err)
		require.Len(t, members, 1)
		require.Equal(t, []model.RoleID{f.role.ID()}, members[0].RoleIDs())
		require.Equal(t, model.StatusActive, members[0].Status())
		got, err := store.GetApplication(ctx, app.ID())
		require.NoError(t, err)
		require.Equal(t, migrated.ID(), got.TeamID())
		_, err = store.GetQuota(ctx, model.QuotaScopeTeam, string(migrated.ID()))
		require.NoError(t, err)
		_, err = store.GetQuota(ctx, model.QuotaScopeTeamMember, model.TeamMemberScopeID(migrated.ID(), f.user.ID()))
		require.NoError(t, err)

		perms, err := store.ResolveEffectivePermissions(ctx, f.user.ID(), f.org.ID())
		require.NoError(t, err)
		require.True(t, perms.CanUseModel(string(migrated.ID()), "m1", rbac.ModelKindLLM), "nobody loses access")

		events, _ := feedSince(t, store, cursor)
		types := eventTypes(events)
		for _, want := range []string{"team.deleted.v1", "team.created.v1", "team_settings.created.v1", "team_membership.created.v1", "application.updated.v1", "quota.updated.v1"} {
			require.Contains(t, types, want)
		}
		item, err := store.ReadProjection(ctx, model.CommonScope{Family: model.FamilyTeamMembership, TenantID: string(testTenantID), OrganizationID: string(f.org.ID()), TeamID: string(migrated.ID())}, string(f.user.ID()))
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"active"}`, string(item.Representation))

		// Run again, the migration changes nothing.
		cursor, err = store.CaptureEventCursor(ctx)
		require.NoError(t, err)
		require.NoError(t, db.Transaction(func(tx *gormpkg.DB) error { return xologorm.MigrateTeamContract(tx) }))
		events, _ = feedSince(t, store, cursor)
		require.Empty(t, events)
	})
}
