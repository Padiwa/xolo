package gorm

import (
	"fmt"

	"github.com/xolo-gateway/xolo/internal/core/port"
	"gorm.io/gorm"
)

// No uniqueIndex tags on PluginNodeSecret: historical AutoMigrate calls must
// not create the new index before the diagnostic checks. Never rewrite rows:
// owners, ciphertext, IDs and timestamps are preserved, including legacy damage.
func migratePluginSecretScope(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var rows []struct {
			ID, OrgID, PluginName, NodeID, Key string
		}
		if err := tx.Table("plugin_node_secrets").Select("id, org_id, plugin_name, node_id, key").Order("id").Find(&rows).Error; err != nil {
			return err
		}
		seen := make(map[[4]string]string, len(rows))
		for _, row := range rows {
			if err := port.ValidateSecretKey(row.OrgID, row.PluginName, row.NodeID, row.Key); err != nil {
				return fmt.Errorf("plugin secret row %q has invalid isolation components; repair manually before retrying: %w", row.ID, err)
			}
			key := [4]string{row.OrgID, row.PluginName, row.NodeID, row.Key}
			if previous, ok := seen[key]; ok {
				return fmt.Errorf("plugin secret rows %q and %q have duplicate isolation keys; repair manually before retrying", previous, row.ID)
			}
			seen[key] = row.ID
		}
		if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_plugin_node_secret_scope_plugin_node_key ON plugin_node_secrets (org_id, plugin_name, node_id, key)").Error; err != nil {
			return err
		}
		return tx.Exec("DROP INDEX IF EXISTS idx_plugin_node_secret_node_key").Error
	})
}
