package gorm

import (
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"gorm.io/gorm"
)

const teamContractMigrationID = "202610200001"

// teamIDColumns lists the columns holding a team ID besides the team tables:
// a renamed team keeps everything bound to it.
var teamIDColumns = []struct{ table, column string }{
	{"team_members", "team_id"},
	{"team_member_roles", "team_id"},
	{"applications", "team_id"},
	{"auth_tokens", "team_id"},
	{"invite_tokens", "team_id"},
	{"personal_virtual_models", "team_id"},
	{"usage_records", "team_id"},
}

// migrateTeamContract moves teams to the common provisioning contract, which
// identifies a team by a UUID and publishes three families: the team (slug,
// name, status), its memberships (status), and its settings under /v1/xolo.
//
// Teams and team members gain a status, active for every existing row. A
// team created locally with an xid gets a UUID: its grants, middlewares,
// members, roles, applications, tokens, invitations, personal models, usage
// records and budgets follow it, and the feed announces the deletion of the
// former key and the creation of the new one. The other teams are published
// again in their new shape.
//
// The migration must run with the other instances stopped: an older instance
// would still publish teams with their members embedded.
func migrateTeamContract(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&Team{}, &TeamMember{}); err != nil {
		return errors.WithStack(err)
	}
	var ids []string
	if err := tx.Model(&Team{}).Order("id").Pluck("id", &ids).Error; err != nil {
		return errors.WithStack(err)
	}
	before := map[mutationKey][]byte{}
	var keys []mutationKey
	remember := func(kind, id string) error {
		key := mutationKey{kind, id}
		if _, seen := before[key]; seen {
			return nil
		}
		raw, err := mutationSnapshot(tx, key)
		if err != nil {
			return err
		}
		before[key] = raw
		keys = append(keys, key)
		return nil
	}
	created := func(kind, id string) {
		key := mutationKey{kind, id}
		before[key] = []byte("null")
		keys = append(keys, key)
	}
	for _, id := range ids {
		if err := remember("team", id); err != nil {
			return err
		}
		teamID := id
		if _, err := model.ParseTeamID(id); err != nil {
			teamID = uuid.NewString()
			if err := rememberTeamDependents(tx, id, remember); err != nil {
				return err
			}
			if err := renameTeam(tx, id, teamID); err != nil {
				return err
			}
			created("team", teamID)
		}
		created("team_settings", teamID)
		var members []string
		if err := tx.Model(&TeamMember{}).Where("team_id = ?", teamID).Order("membership_id").Pluck("membership_id", &members).Error; err != nil {
			return errors.WithStack(err)
		}
		for _, membership := range members {
			created("team_member", teamMemberKey(model.TeamID(teamID), model.MembershipID(membership)))
		}
	}
	return publishProjections(tx.Statement.Context, tx, nil, before, keys)
}

// rememberTeamDependents snapshots the applications and budgets whose
// projection names the team, before its ID changes.
func rememberTeamDependents(tx *gorm.DB, teamID string, remember func(kind, id string) error) error {
	var apps []string
	if err := tx.Model(&Application{}).Where("team_id = ?", teamID).Order("id").Pluck("id", &apps).Error; err != nil {
		return errors.WithStack(err)
	}
	for _, id := range apps {
		if err := remember("application", id); err != nil {
			return err
		}
	}
	var quotas []string
	if err := tx.Model(&Quota{}).Where("(scope = ? AND scope_id = ?) OR (scope = ? AND scope_id LIKE ?)",
		string(model.QuotaScopeTeam), teamID, string(model.QuotaScopeTeamMember), teamID+":%").Order("id").Pluck("id", &quotas).Error; err != nil {
		return errors.WithStack(err)
	}
	for _, id := range quotas {
		if err := remember("quota", id); err != nil {
			return err
		}
	}
	return nil
}

// renameTeam gives the team a new ID. Its grants and middlewares reference
// it through foreign keys: they are recreated rather than updated in place.
func renameTeam(tx *gorm.DB, from, to string) error {
	var grants []TeamModelGrant
	if err := tx.Where("team_id = ?", from).Find(&grants).Error; err != nil {
		return errors.WithStack(err)
	}
	var middlewares []TeamMiddleware
	if err := tx.Where("team_id = ?", from).Find(&middlewares).Error; err != nil {
		return errors.WithStack(err)
	}
	if err := tx.Where("team_id = ?", from).Delete(&TeamModelGrant{}).Error; err != nil {
		return errors.WithStack(err)
	}
	if err := tx.Where("team_id = ?", from).Delete(&TeamMiddleware{}).Error; err != nil {
		return errors.WithStack(err)
	}
	if err := tx.Table("teams").Where("id = ?", from).UpdateColumn("id", to).Error; err != nil {
		return errors.WithStack(err)
	}
	for i := range grants {
		grants[i].TeamID = to
	}
	if len(grants) > 0 {
		if err := tx.Create(&grants).Error; err != nil {
			return errors.WithStack(err)
		}
	}
	for i := range middlewares {
		middlewares[i].TeamID = to
	}
	if len(middlewares) > 0 {
		if err := tx.Create(&middlewares).Error; err != nil {
			return errors.WithStack(err)
		}
	}
	for _, c := range teamIDColumns {
		if !tx.Migrator().HasColumn(c.table, c.column) {
			continue
		}
		if err := tx.Table(c.table).Where(c.column+" = ?", from).UpdateColumn(c.column, to).Error; err != nil {
			return errors.WithStack(err)
		}
	}
	// Budgets and their spend are scoped by the team ID, and a member budget
	// by "<team id>:<user id>".
	for _, table := range []string{"quota", quotaUsageTable} {
		if err := tx.Table(table).Where("scope = ? AND scope_id = ?", string(model.QuotaScopeTeam), from).
			UpdateColumn("scope_id", to).Error; err != nil {
			return errors.WithStack(err)
		}
		var scopeIDs []string
		if err := tx.Table(table).Where("scope = ? AND scope_id LIKE ?", string(model.QuotaScopeTeamMember), from+":%").
			Distinct("scope_id").Pluck("scope_id", &scopeIDs).Error; err != nil {
			return errors.WithStack(err)
		}
		for _, scopeID := range scopeIDs {
			if err := tx.Table(table).Where("scope = ? AND scope_id = ?", string(model.QuotaScopeTeamMember), scopeID).
				UpdateColumn("scope_id", to+strings.TrimPrefix(scopeID, from)).Error; err != nil {
				return errors.WithStack(err)
			}
		}
	}
	return nil
}
