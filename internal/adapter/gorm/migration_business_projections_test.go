package gorm

import (
	"testing"

	"github.com/ncruces/go-sqlite3/gormlite"
	"github.com/stretchr/testify/require"
	"github.com/xolo-gateway/xolo/internal/core/model"
	gormpkg "gorm.io/gorm"
)

// TestBusinessProjectionsBeforeTeams replays the backfill on a schema that
// predates the teams migration, which runs after it and adds the columns
// applications.team_id and quota.cascade.
func TestBusinessProjectionsBeforeTeams(t *testing.T) {
	db, err := gormpkg.Open(gormlite.Open(":memory:"), &gormpkg.Config{})
	require.NoError(t, err)
	require.NoError(t, MigrateDatabase(t.Context(), db, nil))

	var tenant Tenant
	require.NoError(t, db.First(&tenant, "slug = ?", model.DefaultTenantSlug).Error)
	org := model.NewOrganization(model.TenantID(tenant.ID), "acme", "Acme", "")
	require.NoError(t, NewStore(db).CreateOrg(t.Context(), org))
	require.NoError(t, db.Create(&Application{ID: "app-1", OrgID: string(org.ID()), Name: "App", Active: true}).Error)
	require.NoError(t, db.Create(&Quota{ID: "quota-1", Scope: "org", ScopeID: string(org.ID()), Currency: "EUR"}).Error)

	require.NoError(t, db.Migrator().DropIndex(&Application{}, "TeamID"))
	require.NoError(t, db.Migrator().DropColumn(&Application{}, "team_id"))
	require.NoError(t, db.Migrator().DropColumn(&Quota{}, "cascade"))

	require.NoError(t, db.Transaction(func(tx *gormpkg.DB) error { return migrateBusinessProjections(tx) }))
}
