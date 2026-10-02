package api

import (
	"net/http"
	"slices"

	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/core/rbac"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authz"
)

// hasPermission first binds orgID to the current tenant and URL organization,
// then checks the principal's permission. Platform admins and owners bypass
// permissions only, never the parent checks.
//
// Resolution goes through the context resolver, which knows how to resolve both
// members (via their membership) and applications (via the roles assigned to
// the application itself).
func (h *Handler) hasPermission(r *http.Request, orgID model.OrgID, perm rbac.Permission) (bool, error) {
	ctx := r.Context()
	user := httpCtx.User(ctx)
	if user == nil {
		return false, nil
	}
	tenantID := httpCtx.TenantID(ctx)
	if tenantID == "" || user.TenantID() != tenantID {
		return false, nil
	}
	org, err := h.orgStore.GetOrgBySlug(ctx, tenantID, r.PathValue("orgSlug"))
	if errors.Is(err, port.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, errors.WithStack(err)
	}
	if org.TenantID() != tenantID || org.ID() != orgID {
		return false, nil
	}
	// The shadow user backing an application never inherits the platform admin
	// bypass: its roles are an artefact of token authentication.
	if user.Provider() != model.ApplicationProvider && slices.Contains(user.Roles(), authz.RoleAdmin) {
		return true, nil
	}

	set, err := httpCtx.ResolvePermissions(ctx, orgID)
	if err != nil {
		return false, errors.WithStack(err)
	}

	return set.IsOwner() || set.Has(perm), nil
}
