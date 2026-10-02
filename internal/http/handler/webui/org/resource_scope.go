package org

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/core/service"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
)

// Resource IDs are global. Permission on the URL organization never authorizes
// a resource until its parent chain has been checked, including for platform
// admins. Resolve before rendering, reading secrets or quotas, or mutating.

func (h *Handler) resolveOrgAndProvider(ctx context.Context, orgSlug, id string) (model.Organization, model.Provider, error) {
	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	resource, err := h.providerStore.GetProviderByID(ctx, model.ProviderID(id))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	if resource.OrgID() != org.ID() {
		return nil, nil, errors.WithStack(port.ErrNotFound)
	}
	return org, resource, nil
}

func (h *Handler) resolveOrgAndVirtualModel(ctx context.Context, orgSlug, id string) (model.Organization, model.VirtualModel, error) {
	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	resource, err := h.virtualModelStore.GetVirtualModelByID(ctx, model.VirtualModelID(id))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	if resource.OrgID() != org.ID() {
		return nil, nil, errors.WithStack(port.ErrNotFound)
	}
	return org, resource, nil
}

func (h *Handler) resolveOrgAndMiddleware(ctx context.Context, orgSlug, id string) (model.Organization, model.Middleware, error) {
	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	resource, err := h.middlewareStore.GetMiddlewareByID(ctx, model.MiddlewareID(id))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	if resource.OrgID() != org.ID() {
		return nil, nil, errors.WithStack(port.ErrNotFound)
	}
	return org, resource, nil
}

func (h *Handler) resolveOrgAndMembership(ctx context.Context, orgSlug, id string) (model.Organization, model.Membership, error) {
	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	resource, err := h.orgStore.GetMembership(ctx, model.MembershipID(id))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	if resource.OrgID() != org.ID() {
		return nil, nil, errors.WithStack(port.ErrNotFound)
	}
	return org, resource, nil
}

func (h *Handler) resolveOrgAndProviderModel(ctx context.Context, orgSlug, providerID, modelID string) (model.Organization, model.Provider, model.LLMModel, error) {
	org, provider, err := h.resolveOrgAndProvider(ctx, orgSlug, providerID)
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := h.providerStore.GetLLMModelByID(ctx, model.LLMModelID(modelID))
	if err != nil {
		return nil, nil, nil, errors.WithStack(err)
	}
	if m.OrgID() != org.ID() || m.ProviderID() != provider.ID() {
		return nil, nil, nil, errors.WithStack(port.ErrNotFound)
	}
	return org, provider, m, nil
}

func (h *Handler) resolveOrgAndInvite(ctx context.Context, orgSlug, inviteID string) (model.Organization, model.InviteToken, error) {
	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	resolver := service.NewInvitationResolver(h.inviteStore, h.orgStore)
	invite, _, err := resolver.ResolveInOrg(ctx, httpCtx.TenantID(ctx), org.ID(), model.InviteTokenID(inviteID))
	if err != nil {
		return nil, nil, err
	}
	return org, invite, nil
}

// Unknown and foreign resources have exactly the same response.
func writeResourceLookupError(ctx context.Context, w http.ResponseWriter, err error) {
	if errors.Is(err, port.ErrNotFound) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	slog.ErrorContext(ctx, "could not resolve organization resource", slog.Any("error", err))
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}
