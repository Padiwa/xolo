package gorm

import (
	"fmt"
	"time"

	"github.com/rs/xid"
	"gorm.io/gorm"
)

// No uniqueIndex model tag: older AutoMigrate calls must not try to create this
// index before the explicit duplicate check has produced an actionable error.
func migrateUniqueMemberships(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var duplicates []struct {
			UserID string
			OrgID  string
			Count  int64
		}
		if err := tx.Model(&Membership{}).Select("user_id, org_id, COUNT(*) AS count").
			Group("user_id, org_id").Having("COUNT(*) > 1").Limit(20).Find(&duplicates).Error; err != nil {
			return err
		}
		if len(duplicates) > 0 {
			return fmt.Errorf("duplicate memberships prevent unique (user_id, org_id) index; review memberships and membership_roles manually before retrying (first 20 groups): %+v", duplicates)
		}
		return tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_memberships_user_org ON memberships (user_id, org_id)").Error
	})
}

func migrateRevokeLegacyInvitations(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var ids []string
		now := time.Now()
		if err := tx.Model(&InviteToken{}).Where("revoked_at IS NULL AND LENGTH(id) = 20").
			Scopes(unexpiredInvitations(now)).
			Where("max_uses IS NULL OR uses_count < max_uses").
			Where("invitee_email IS NULL OR uses_count = 0").Pluck("id", &ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := xid.FromString(id); err != nil {
				continue
			}
			if err := tx.Model(&InviteToken{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", now).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
