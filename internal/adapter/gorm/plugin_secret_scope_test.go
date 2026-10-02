package gorm_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	"github.com/xolo-gateway/xolo/internal/core/port"
	gormpkg "gorm.io/gorm"
)

func TestPluginSecretIsolation(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		store := newStoreOn(t, db)
		ctx := t.Context()
		// Organization and user IDs are globally unique, including across tenants.
		scopes := []string{"tenant-a-org-1", "tenant-a-org-2", "tenant-b-org-1", "~:user-a", "~:user-b", " ~:user-a "}
		plugins := []string{"pseudonymizer", "mcp-bridge"}
		for _, scope := range scopes {
			for _, plugin := range plugins {
				require.NoError(t, store.SetSecret(ctx, scope, plugin, "same-node", "same-key", scope+plugin))
				require.NoError(t, store.SetSecret(ctx, scope, plugin, "same-node", "other-key", ""))
			}
		}
		var before []xologorm.PluginNodeSecret
		require.NoError(t, db.Order("id").Find(&before).Error)
		require.Len(t, before, len(scopes)*len(plugins)*2)
		for _, row := range before {
			original, found, err := store.GetSecret(ctx, row.OrgID, row.PluginName, row.NodeID, row.Key)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, row.ValueEncrypted, original)
		}
		for _, row := range before {
			require.NoError(t, store.SetSecret(ctx, row.OrgID, row.PluginName, row.NodeID, row.Key, "updated-"+row.OrgID+row.PluginName))
			var after xologorm.PluginNodeSecret
			require.NoError(t, db.First(&after, "id = ?", row.ID).Error)
			require.Equal(t, row.OrgID, after.OrgID)
			require.Equal(t, row.PluginName, after.PluginName)
			require.Equal(t, row.NodeID, after.NodeID)
			require.Equal(t, row.Key, after.Key)
			require.True(t, row.CreatedAt.Equal(after.CreatedAt))
			require.False(t, after.UpdatedAt.Before(row.UpdatedAt))
			value, found, err := store.GetSecret(ctx, row.OrgID, row.PluginName, row.NodeID, row.Key)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "updated-"+row.OrgID+row.PluginName, value)
		}
		for _, key := range [][4]string{
			{"unknown", plugins[0], "same-node", "same-key"},
			{scopes[0], "unknown", "same-node", "same-key"},
			{scopes[0], plugins[0], "unknown", "same-key"},
			{scopes[0], plugins[0], "same-node", "unknown"},
		} {
			_, found, err := store.GetSecret(ctx, key[0], key[1], key[2], key[3])
			require.NoError(t, err)
			require.False(t, found)
			require.NoError(t, store.DeleteSecret(ctx, key[0], key[1], key[2], key[3]))
		}
		require.NoError(t, store.DeleteSecret(ctx, scopes[0], plugins[0], "same-node", "same-key"))
		require.NoError(t, store.DeleteAllForNode(ctx, scopes[1], plugins[1], "same-node"))
		require.NoError(t, store.DeleteAllForNode(ctx, scopes[1], plugins[1], "same-node"))
		for _, row := range before {
			_, found, err := store.GetSecret(ctx, row.OrgID, row.PluginName, row.NodeID, row.Key)
			require.NoError(t, err)
			deleted := row.OrgID == scopes[0] && row.PluginName == plugins[0] && row.Key == "same-key" || row.OrgID == scopes[1] && row.PluginName == plugins[1]
			require.Equal(t, !deleted, found)
		}
	})
}

func TestPluginSecretValidation(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		ctx := t.Context()
		for component := range 4 {
			for _, invalid := range []string{"", " \t\n\u2003"} {
				t.Run(fmt.Sprintf("component-%d-%q", component, invalid), func(t *testing.T) {
					key := [4]string{"org", "plugin", "node", "key"}
					key[component] = invalid
					_, found, err := store.GetSecret(ctx, key[0], key[1], key[2], key[3])
					require.ErrorIs(t, err, port.ErrInvalid)
					require.False(t, found)
					require.ErrorIs(t, store.SetSecret(ctx, key[0], key[1], key[2], key[3], ""), port.ErrInvalid)
					require.ErrorIs(t, store.DeleteSecret(ctx, key[0], key[1], key[2], key[3]), port.ErrInvalid)
					if component < 3 {
						require.ErrorIs(t, store.DeleteAllForNode(ctx, key[0], key[1], key[2]), port.ErrInvalid)
					}
				})
			}
		}
		require.NoError(t, store.SetSecret(ctx, "org", "plugin", "node", "key", ""))
		value, found, err := store.GetSecret(ctx, "org", "plugin", "node", "key")
		require.NoError(t, err)
		require.True(t, found)
		require.Empty(t, value)
	})
}

func TestPluginSecretConcurrentWrites(t *testing.T) {
	eachBackendDB(t, func(t *testing.T, db *gormpkg.DB) {
		ctx := t.Context()
		store := newStoreOn(t, db)
		require.NoError(t, store.Migrate(ctx))
		pool, err := db.DB()
		require.NoError(t, err)
		pool.SetMaxOpenConns(8) // SQLite uses a file-backed WAL, like PostgreSQL a shared DB.
		start := make(chan struct{})
		errs := make(chan error, 24)
		var wg sync.WaitGroup
		for i := range 24 {
			wg.Go(func() {
				<-start
				scope := fmt.Sprintf("scope-%d", i%3)
				err := store.SetSecret(ctx, scope, "plugin", "node", "key", fmt.Sprint(i))
				if err == nil && i%3 == 0 {
					err = store.DeleteAllForNode(ctx, scope, "plugin", "node")
				}
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		var rows []xologorm.PluginNodeSecret
		require.NoError(t, db.Find(&rows).Error)
		require.Len(t, rows, 2)
		for _, scope := range []string{"scope-1", "scope-2"} {
			_, found, err := store.GetSecret(ctx, scope, "plugin", "node", "key")
			require.NoError(t, err)
			require.True(t, found)
		}
	})
}
