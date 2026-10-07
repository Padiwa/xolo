package gorm

import (
	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"gorm.io/gorm"
)

const inviteEmailMigrationID = "202610070002"

// migrateNormalizeInviteeEmails brings the invitations written before
// model.NormalizeEmail to the form every reader now expects. A blank address
// stays targeted and matches nobody, as it always did: turning it into NULL
// would open it to anyone. It runs model.NormalizeEmail itself: SQLite's
// LOWER() only folds ASCII and TRIM() only strips spaces, so SQL alone would
// leave rows the plain-equality lookup can no longer find.
func migrateNormalizeInviteeEmails(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&InviteToken{}) {
		return nil
	}
	var rows []InviteToken
	if err := tx.Select("id", "invitee_email").Where("invitee_email IS NOT NULL").Find(&rows).Error; err != nil {
		return errors.WithStack(err)
	}
	for _, row := range rows {
		normalized := model.NormalizeEmail(*row.InviteeEmail)
		if normalized == *row.InviteeEmail {
			continue
		}
		if err := tx.Model(&InviteToken{}).Where("id = ?", row.ID).Update("invitee_email", normalized).Error; err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}
