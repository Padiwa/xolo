package service

import (
	"context"
	"slices"
	"strings"

	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

// teamParent checks, within the transaction, the tenant and the organization
// a team hangs from.
func (s *ProvisioningService) teamParent(ctx context.Context, tenantID model.TenantID, orgID model.OrgID) error {
	if _, err := s.commonParentTenant(ctx, tenantID); err != nil {
		return err
	}
	org, err := s.orgStore.GetOrgByID(ctx, orgID)
	if errors.Is(err, port.ErrNotFound) || (err == nil && org.TenantID() != tenantID) {
		return errors.Wrap(port.ErrParentNotFound, "organization not found")
	}
	return errors.WithStack(err)
}

// organizationTeam loads a team of the organization: a missing team, or one
// of another organization, is a missing parent.
func (s *ProvisioningService) organizationTeam(ctx context.Context, orgID model.OrgID, teamID model.TeamID) (model.Team, error) {
	team, err := s.tx.GetTeamByID(ctx, teamID)
	if errors.Is(err, port.ErrNotFound) || (err == nil && team.OrgID() != orgID) {
		return nil, errors.Wrap(port.ErrParentNotFound, "team not found")
	}
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return team, nil
}

// PutTeam creates the team of the common contract or brings its slug, name
// and status to the given representation. Its settings are left untouched; a
// new team has none. A team ID is global: one held by another organization is
// a conflict. A new slug is checked like the slug of a new team; the model
// names clients stored with the former one stop resolving.
func (s *ProvisioningService) PutTeam(ctx context.Context, tenantID model.TenantID, orgID model.OrgID, teamID model.TeamID, p CommonResource) (CommonResource, error) {
	p, err := normalizeCommonResource(p)
	if err != nil {
		return p, err
	}
	if !s.bound {
		ctx = model.EnsureActor(ctx)
		return p, s.transaction(ctx, func(tx *ProvisioningService) error {
			_, err := tx.PutTeam(ctx, tenantID, orgID, teamID, p)
			return err
		})
	}

	if err := s.teamParent(ctx, tenantID, orgID); err != nil {
		return p, err
	}
	old, err := s.tx.GetTeamByID(ctx, teamID)
	if errors.Is(err, port.ErrNotFound) {
		if err := CheckTeamSlug(ctx, s.tx, orgID, p.Slug); err != nil {
			return p, err
		}
		team := model.NewTeam(orgID, p.Slug, p.Name, "")
		team.SetID(teamID)
		team.SetStatus(p.Status)
		return p, errors.WithStack(s.tx.CreateTeam(ctx, team))
	}
	if err != nil {
		return p, errors.WithStack(err)
	}
	if old.OrgID() != orgID {
		return p, errors.Wrap(port.ErrAlreadyExists, "team id is used by another organization")
	}
	if old.Slug() == p.Slug && old.Name() == p.Name && old.Status() == p.Status {
		return p, nil
	}
	if old.Slug() != p.Slug {
		if err := CheckTeamSlug(ctx, s.tx, orgID, p.Slug); err != nil {
			return p, err
		}
	}
	team := model.CopyTeam(old)
	team.SetSlug(p.Slug)
	team.SetName(p.Name)
	team.SetStatus(p.Status)
	return p, errors.WithStack(s.tx.SaveTeam(ctx, team))
}

// PutTeamMembership places the organization member in the team, or changes
// its status. The member needs an organization membership in the team's
// organization, whatever its status. A new team member holds no team role;
// an existing one keeps its roles.
func (s *ProvisioningService) PutTeamMembership(ctx context.Context, tenantID model.TenantID, orgID model.OrgID, teamID model.TeamID, userID model.UserID, status model.Status) (model.Status, error) {
	if !status.Valid() {
		return status, errors.WithStack(port.ErrInvalid)
	}
	if !s.bound {
		ctx = model.EnsureActor(ctx)
		return status, s.transaction(ctx, func(tx *ProvisioningService) error {
			_, err := tx.PutTeamMembership(ctx, tenantID, orgID, teamID, userID, status)
			return err
		})
	}

	if err := s.teamParent(ctx, tenantID, orgID); err != nil {
		return status, err
	}
	if _, err := s.organizationTeam(ctx, orgID, teamID); err != nil {
		return status, err
	}
	membership, err := s.orgStore.GetUserOrgMembership(ctx, userID, orgID)
	if errors.Is(err, port.ErrNotFound) {
		return status, errors.Wrap(port.ErrParentNotFound, "user is no member of the organization")
	}
	if err != nil {
		return status, errors.WithStack(err)
	}
	members, err := s.tx.ListTeamMembers(ctx, teamID)
	if err != nil {
		return status, errors.WithStack(err)
	}
	i := slices.IndexFunc(members, func(m model.TeamMember) bool { return m.MembershipID() == membership.ID() })
	switch {
	case i < 0:
		return status, errors.WithStack(s.tx.AddTeamMember(ctx, teamID, membership.ID(), status))
	case members[i].Status() == status:
		return status, nil
	}
	return status, errors.WithStack(s.tx.SetTeamMemberStatus(ctx, teamID, membership.ID(), status))
}

// DeleteTeam removes the team at once, with its members, settings and
// budgets. A team an application is bound to, or alone holding a middleware,
// is not deleted. Deleting a missing team changes nothing.
func (s *ProvisioningService) DeleteTeam(ctx context.Context, tenantID model.TenantID, orgID model.OrgID, teamID model.TeamID, condition model.MatchCondition) error {
	scope := model.CommonScope{Family: model.FamilyTeam, TenantID: string(tenantID), OrganizationID: string(orgID)}
	ctx = model.EnsureActor(ctx)
	return s.transaction(ctx, func(tx *ProvisioningService) error {
		exists, err := tx.deleteTarget(ctx, scope, string(teamID), condition)
		if err != nil || !exists {
			return err
		}
		return errors.WithStack(tx.tx.DeleteTeam(ctx, teamID))
	})
}

// DeleteTeamMembership removes the member from the team at once, with its
// team roles and budget. Deleting a missing team membership changes nothing.
func (s *ProvisioningService) DeleteTeamMembership(ctx context.Context, tenantID model.TenantID, orgID model.OrgID, teamID model.TeamID, userID model.UserID, condition model.MatchCondition) error {
	scope := model.CommonScope{Family: model.FamilyTeamMembership, TenantID: string(tenantID), OrganizationID: string(orgID), TeamID: string(teamID)}
	ctx = model.EnsureActor(ctx)
	return s.transaction(ctx, func(tx *ProvisioningService) error {
		exists, err := tx.deleteTarget(ctx, scope, string(userID), condition)
		if err != nil || !exists {
			return err
		}
		membership, err := tx.orgStore.GetUserOrgMembership(ctx, userID, orgID)
		if err != nil {
			return errors.WithStack(err)
		}
		return errors.WithStack(tx.tx.RemoveTeamMember(ctx, teamID, membership.ID()))
	})
}

// PutTeamSettings brings the settings of a team to the given representation:
// its description, budget sharing, model grants, middlewares and the roles
// its members hold on the team scope. The settings exist exactly when the
// team does, which the common contract creates.
func (s *ProvisioningService) PutTeamSettings(ctx context.Context, tenantID model.TenantID, orgID model.OrgID, key string, condition model.MatchCondition, v model.TeamSettings) (model.CommonItem, error) {
	teamID, err := model.ParseTeamID(key)
	if err != nil {
		return model.CommonItem{}, errors.WithStack(port.ErrInvalid)
	}
	if err := normalizeTeamSettings(&v); err != nil {
		return model.CommonItem{}, err
	}
	scope := model.CommonScope{Family: model.FamilyTeamSettings, TenantID: string(tenantID), OrganizationID: string(orgID)}
	return s.PutCommon(ctx, scope, key, condition, func(ctx context.Context, tx *ProvisioningService) error {
		if err := tx.teamParent(ctx, tenantID, orgID); err != nil {
			return err
		}
		current, err := tx.organizationTeam(ctx, orgID, teamID)
		if err != nil {
			return err
		}
		item, err := tx.tx.ReadProjection(ctx, scope, key)
		if err != nil {
			return errors.WithStack(err)
		}
		if unchanged, err := sameRepresentation(v, item.Representation); err != nil || unchanged {
			return err
		}

		grants := make([]model.ModelGrant, 0, len(v.ModelGrants))
		for _, grant := range v.ModelGrants {
			if err := tx.checkModelGrant(ctx, orgID, grant); err != nil {
				return err
			}
			grants = append(grants, model.ModelGrant{ModelID: grant.ModelID, Kind: grant.Kind})
		}
		middlewares := make([]model.MiddlewareID, 0, len(v.MiddlewareIDs))
		for _, id := range v.MiddlewareIDs {
			mw, err := tx.tx.GetMiddlewareByID(ctx, model.MiddlewareID(id))
			if errors.Is(err, port.ErrNotFound) || (err == nil && mw.OrgID() != orgID) {
				return errors.Wrapf(port.ErrParentNotFound, "middleware %q not found", id)
			}
			if err != nil {
				return errors.WithStack(err)
			}
			middlewares = append(middlewares, mw.ID())
		}

		// Roles are resolved before any write: a member or role that does
		// not resolve fails the whole representation.
		members, err := tx.tx.ListTeamMembers(ctx, teamID)
		if err != nil {
			return errors.WithStack(err)
		}
		wanted := map[model.MembershipID][]model.RoleID{}
		for _, m := range v.MemberRoles {
			i := slices.IndexFunc(members, func(member model.TeamMember) bool { return string(member.UserID()) == m.UserID })
			if i < 0 {
				return errors.Wrapf(port.ErrParentNotFound, "user %q is no member of the team", m.UserID)
			}
			roles := make([]model.RoleID, 0, len(m.RoleIDs))
			for _, id := range m.RoleIDs {
				role, err := tx.roleStore.GetRoleByID(ctx, model.RoleID(id))
				if errors.Is(err, port.ErrNotFound) || (err == nil && role.OrgID() != orgID) {
					return errors.Wrapf(port.ErrParentNotFound, "role %q not found", id)
				}
				if err != nil {
					return errors.WithStack(err)
				}
				// Like the web UI, never hold a builtin role on a team: its
				// codes would be cut to the team-scopable ones without a word.
				if role.Builtin() {
					return errors.Wrapf(port.ErrInvalid, "builtin role %q cannot be held on a team", id)
				}
				roles = append(roles, role.ID())
			}
			wanted[members[i].MembershipID()] = roles
		}

		team := model.CopyTeam(current)
		team.SetDescription(v.Description)
		team.SetShareQuotaEqually(v.ShareQuotaEqually)
		team.SetModelGrants(grants)
		team.SetMiddlewareIDs(middlewares)
		if err := tx.tx.SaveTeam(ctx, team); err != nil {
			return errors.WithStack(err)
		}
		for _, member := range members {
			roles := wanted[member.MembershipID()]
			if slices.Equal(roles, member.RoleIDs()) || (len(roles) == 0 && len(member.RoleIDs()) == 0) {
				continue
			}
			if err := tx.tx.SetTeamMember(ctx, teamID, member.MembershipID(), roles); err != nil {
				return errors.WithStack(err)
			}
		}
		return nil
	})
}

// normalizeTeamSettings validates team settings and sorts their lists, so
// that they compare with their projection. Members listed without a role are
// dropped: the projection lists only the members holding one.
func normalizeTeamSettings(v *model.TeamSettings) error {
	if !validCommonText(v.Description, 0, maxBusinessDescriptionLength) ||
		v.ModelGrants == nil || v.MiddlewareIDs == nil || v.MemberRoles == nil {
		return errors.WithStack(port.ErrInvalid)
	}
	grants := make([]model.ModelGrant, 0, len(v.ModelGrants))
	for _, g := range v.ModelGrants {
		grants = append(grants, model.ModelGrant{ModelID: g.ModelID, Kind: g.Kind})
	}
	if err := validateModelGrants(grants); err != nil {
		return err
	}
	v.ModelGrants = make([]model.ModelGrantSettings, 0, len(grants))
	for _, g := range model.NormalizeModelGrants(grants) {
		v.ModelGrants = append(v.ModelGrants, model.ModelGrantSettings{ModelID: g.ModelID, Kind: g.Kind})
	}
	v.MiddlewareIDs = sortedUnique(v.MiddlewareIDs)
	members := make([]model.TeamMemberSettings, 0, len(v.MemberRoles))
	for _, m := range v.MemberRoles {
		if _, err := model.ParseUserID(m.UserID); err != nil {
			return errors.Wrap(port.ErrInvalid, "invalid member user id")
		}
		if m.RoleIDs == nil {
			return errors.WithStack(port.ErrInvalid)
		}
		if len(m.RoleIDs) > 0 {
			members = append(members, model.TeamMemberSettings{UserID: m.UserID, RoleIDs: sortedUnique(m.RoleIDs)})
		}
	}
	slices.SortFunc(members, func(a, b model.TeamMemberSettings) int { return strings.Compare(a.UserID, b.UserID) })
	for i := 1; i < len(members); i++ {
		if members[i].UserID == members[i-1].UserID {
			return errors.Wrapf(port.ErrInvalid, "user %q is listed twice", members[i].UserID)
		}
	}
	v.MemberRoles = members
	return nil
}
