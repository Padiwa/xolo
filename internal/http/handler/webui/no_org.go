package webui

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/a-h/templ"
	"github.com/bornholm/go-x/slogx"
	"github.com/xolo-gateway/xolo/internal/core/model"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/handler/webui/common"
	"github.com/xolo-gateway/xolo/internal/http/handler/webui/profile/component"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authz"
)

func (h *Handler) getNoOrgPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := httpCtx.User(ctx)
	memberships := httpCtx.Memberships(ctx)

	baseURL := httpCtx.BaseURL(ctx)

	// If user has any membership, redirect to /usage
	if len(memberships) > 0 {
		http.Redirect(w, r, baseURL.JoinPath("/usage").String(), http.StatusTemporaryRedirect)
		return
	}

	// Fetch pending invitations for the user's email
	invites, err := h.inviteStore.ListPendingInvitesForEmail(ctx, httpCtx.TenantID(ctx), user.Email())
	if err != nil {
		slog.ErrorContext(ctx, "could not fetch invitations", slogx.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	// Collect declined invite IDs from cookies
	var declinedIDs []string
	for _, inv := range invites {
		cookieName := fmt.Sprintf("declined_invite_%s", string(inv.ID()))
		if _, err := r.Cookie(cookieName); err == nil {
			declinedIDs = append(declinedIDs, string(inv.ID()))
		}
	}

	invites, names, err := common.PrepareInvitationList(ctx, h.invitationService, httpCtx.TenantID(ctx), user.ID(), invites)
	if err != nil {
		common.WriteInvitationError(w, r, err)
		return
	}
	vmodel := component.NoOrgPageVModel{
		RoleNames:   names,
		User:        user,
		Invites:     invites,
		DeclinedIDs: declinedIDs,
		IsAdmin:     slices.Contains(user.Roles(), authz.RoleAdmin),
	}

	templ.Handler(component.NoOrgPage(vmodel)).ServeHTTP(w, r)
}

func (h *Handler) declineInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	baseURL := httpCtx.BaseURL(ctx)

	tokenID := r.PathValue("tokenID")
	if tokenID == "" {
		http.Error(w, "Token ID is required", http.StatusBadRequest)
		return
	}

	user := httpCtx.User(ctx)
	if user == nil {
		http.Redirect(w, r, "/auth/oidc/login", http.StatusSeeOther)
		return
	}
	open, err := h.invitationService.Decline(ctx, httpCtx.TenantID(ctx), model.InviteTokenID(tokenID), user.ID())
	if err != nil {
		common.WriteInvitationError(w, r, err)
		return
	}
	if open {
		cookieName := fmt.Sprintf("declined_invite_%s", tokenID)
		http.SetCookie(w, &http.Cookie{
			Name:   cookieName,
			Value:  "1",
			Path:   "/",
			MaxAge: 86400,
		})
	}

	http.Redirect(w, r, baseURL.JoinPath("/no-org").String(), http.StatusSeeOther)
}
