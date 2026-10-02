package gorm_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	gormpkg "gorm.io/gorm"
)

const secretScopeIndex = "idx_plugin_node_secret_scope_plugin_node_key"
const legacySecretIndex = "idx_plugin_node_secret_node_key"

func prepareSecretUpgrade(t *testing.T, db *gormpkg.DB, legacyIndex bool) {
	t.Helper()
	require.NoError(t, xologorm.NewStore(db).Migrate(t.Context()))
	require.True(t, db.Migrator().HasIndex(&xologorm.PluginNodeSecret{}, secretScopeIndex))
	require.NoError(t, db.Exec("DROP INDEX "+secretScopeIndex).Error)
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "202609300003").Error)
	if legacyIndex {
		require.NoError(t, db.Exec("CREATE UNIQUE INDEX "+legacySecretIndex+" ON plugin_node_secrets (node_id, key)").Error)
	}
}

func secretMigrationRow(id, scope, node string) xologorm.PluginNodeSecret {
	stamp := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	return xologorm.PluginNodeSecret{ID: id, OrgID: scope, PluginName: "plugin", NodeID: node, Key: "key", ValueEncrypted: "opaque-ciphertext-do-not-print", CreatedAt: stamp, UpdatedAt: stamp}
}

func TestPluginSecretMigrationFreshUpgradeReplay(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		prepareSecretUpgrade(t, db, true)
		rows := []xologorm.PluginNodeSecret{secretMigrationRow("a", "org", "node-a"), secretMigrationRow("b", "~:user", "node-b")}
		require.NoError(t, db.Create(&rows).Error)
		var before []xologorm.PluginNodeSecret
		require.NoError(t, db.Order("id").Find(&before).Error)
		for range 2 {
			require.NoError(t, xologorm.NewStore(db).Migrate(t.Context()))
			require.True(t, db.Migrator().HasIndex(&xologorm.PluginNodeSecret{}, secretScopeIndex))
			require.False(t, db.Migrator().HasIndex(&xologorm.PluginNodeSecret{}, legacySecretIndex))
			var after []xologorm.PluginNodeSecret
			require.NoError(t, db.Order("id").Find(&after).Error)
			require.Equal(t, before, after)
			require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "202609300003").Error)
		}
		store := xologorm.NewStore(db)
		require.NoError(t, store.SetSecret(t.Context(), "other", "plugin", "node-a", "key", "new"))
		require.NoError(t, store.SetSecret(t.Context(), "org", "other-plugin", "node-a", "key", "new"))
		duplicate := secretMigrationRow("duplicate", "org", "node-a")
		require.Error(t, db.Create(&duplicate).Error)
	})
}

func TestPluginSecretMigrationRejectsInvalidAndDuplicateRows(t *testing.T) {
	for _, kind := range []string{"scope", "plugin", "node", "key", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
				prepareSecretUpgrade(t, db, kind != "duplicate")
				row := secretMigrationRow("bad-row", "org", "node")
				switch kind {
				case "scope":
					row.OrgID = "\t\u2003"
				case "plugin":
					row.PluginName = ""
				case "node":
					row.NodeID = "\n"
				case "key":
					row.Key = " "
				}
				require.NoError(t, db.Create(&row).Error)
				if kind == "duplicate" {
					duplicate := row
					duplicate.ID = "duplicate-row"
					require.NoError(t, db.Create(&duplicate).Error)
				}
				var before []xologorm.PluginNodeSecret
				require.NoError(t, db.Order("id").Find(&before).Error)
				err := xologorm.NewStore(db).Migrate(t.Context())
				require.ErrorContains(t, err, "bad-row")
				require.NotContains(t, err.Error(), row.ValueEncrypted)
				if kind == "duplicate" {
					require.ErrorContains(t, err, "duplicate-row")
				}
				require.False(t, db.Migrator().HasIndex(&row, secretScopeIndex))
				require.Equal(t, kind != "duplicate", db.Migrator().HasIndex(&row, legacySecretIndex))
				var after []xologorm.PluginNodeSecret
				require.NoError(t, db.Order("id").Find(&after).Error)
				require.Equal(t, before, after)
				var count int64
				require.NoError(t, db.Table("migrations").Where("id = ?", "202609300003").Count(&count).Error)
				require.Zero(t, count)
			})
		})
	}
}

func TestPluginSecretMigrationRollsBackPartialDDL(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		prepareSecretUpgrade(t, db, true)
		row := secretMigrationRow("preserved", "org", "node")
		require.NoError(t, db.Create(&row).Error)
		require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register("test:fail-secret-drop", func(tx *gormpkg.DB) {
			if strings.Contains(tx.Statement.SQL.String(), "DROP INDEX IF EXISTS "+legacySecretIndex) {
				tx.AddError(errors.New("injected DDL failure"))
			}
		}))
		err := xologorm.NewStore(db).Migrate(t.Context())
		require.NoError(t, db.Callback().Raw().Remove("test:fail-secret-drop"))
		require.ErrorContains(t, err, "injected DDL failure")
		require.True(t, db.Migrator().HasIndex(&row, legacySecretIndex))
		require.False(t, db.Migrator().HasIndex(&row, secretScopeIndex))
		var after xologorm.PluginNodeSecret
		require.NoError(t, db.First(&after, "id = ?", row.ID).Error)
		require.Equal(t, row.ID, after.ID)
		require.Equal(t, row.ValueEncrypted, after.ValueEncrypted)
		require.NoError(t, xologorm.NewStore(db).Migrate(t.Context()))
	})
}
