package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/bornholm/go-x/slogx"
	"github.com/xolo-gateway/xolo/internal/core/model"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn"
)

type userQuotaResolver interface {
	ResolveUserQuota(ctx context.Context, userID model.UserID, orgID model.OrgID) (*model.UserQuota, error)
}

type quotaSpendReader interface {
	SumQuotaCostSince(ctx context.Context, scope model.QuotaScope, scopeID string, orgID model.OrgID, since time.Time) (int64, error)
}

// WithUserQuota serves GET /api/v1/quota, the caller's own budget inside one
// of their organizations. now gives the budget windows.
func (h *Handler) WithUserQuota(quotas userQuotaResolver, spend quotaSpendReader, now func() time.Time) *Handler {
	h.userQuotas = quotas
	h.quotaSpend = spend
	h.now = now
	h.mux.HandleFunc("GET /api/v1/quota", h.handleQuota)
	return h
}

type quotaPeriod struct {
	// Budget and Remaining are null when the period is unlimited.
	Budget    *int64    `json:"budget"`
	Spent     int64     `json:"spent"`
	Remaining *int64    `json:"remaining"`
	ResetsAt  time.Time `json:"resets_at"`
}

type quotaPeriods struct {
	Daily   quotaPeriod `json:"daily"`
	Monthly quotaPeriod `json:"monthly"`
	Yearly  quotaPeriod `json:"yearly"`
}

type quotaResponse struct {
	OrganizationID string       `json:"organization_id"`
	Source         string       `json:"source"`
	Currency       string       `json:"currency"`
	Periods        quotaPeriods `json:"periods"`
}

// handleQuota answers with the budget of the user level only: the one checked
// against the user's own counter. The organization, team and member levels
// keep their own budgets and are not reported here.
func (h *Handler) handleQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := httpCtx.User(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if user.Provider() == model.ApplicationProvider {
		writeError(w, http.StatusForbidden, "an application has no user quota")
		return
	}

	orgID, status, msg := h.quotaOrg(r, user.ID())
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}

	quota, err := h.userQuotas.ResolveUserQuota(ctx, user.ID(), orgID)
	if err != nil {
		slog.ErrorContext(ctx, "could not resolve user quota", slogx.Error(err))
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	now := h.now()
	day, month, year := model.StartOfDay(now), model.StartOfMonth(now), model.StartOfYear(now)
	response := quotaResponse{OrganizationID: string(orgID), Source: string(quota.Source), Currency: quota.Currency}
	for _, p := range []struct {
		target *quotaPeriod
		budget *int64
		start  time.Time
		end    time.Time
	}{
		{&response.Periods.Daily, quota.DailyBudget, day, day.AddDate(0, 0, 1)},
		{&response.Periods.Monthly, quota.MonthlyBudget, month, month.AddDate(0, 1, 0)},
		{&response.Periods.Yearly, quota.YearlyBudget, year, year.AddDate(1, 0, 0)},
	} {
		spent, err := h.quotaSpend.SumQuotaCostSince(ctx, model.QuotaScopeUser, string(user.ID()), orgID, p.start)
		if err != nil {
			slog.ErrorContext(ctx, "could not sum user quota spend", slogx.Error(err))
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		*p.target = quotaPeriod{Budget: p.budget, Spent: spent, ResetsAt: p.end}
		if p.budget != nil {
			remaining := max(*p.budget-spent, 0)
			p.target.Remaining = &remaining
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// quotaOrg resolves the organization of the request: the one an API token is
// bound to, otherwise the organization parameter, otherwise the caller's only
// organization. The caller must be an active member of it: anything else is
// the same 404, whether the organization exists or not.
func (h *Handler) quotaOrg(r *http.Request, userID model.UserID) (model.OrgID, int, string) {
	ctx := r.Context()
	requested := model.OrgID(r.URL.Query().Get("organization"))
	var orgID model.OrgID
	if authnUser := authn.OptionalContextUser(ctx); authnUser != nil && authnUser.OrgID != "" {
		orgID = model.OrgID(authnUser.OrgID)
		if requested != "" && requested != orgID {
			return "", http.StatusNotFound, "organization not found"
		}
	} else {
		orgID = requested
	}

	// Only active memberships of active organizations are returned. The
	// memberships middleware already loaded them, unless its query failed.
	memberships := httpCtx.Memberships(ctx)
	if memberships == nil {
		var err error
		memberships, err = h.orgStore.GetUserMemberships(ctx, userID)
		if err != nil {
			slog.ErrorContext(ctx, "could not fetch user memberships", slogx.Error(err))
			return "", http.StatusInternalServerError, "internal server error"
		}
	}
	if orgID == "" {
		if len(memberships) != 1 {
			return "", http.StatusBadRequest, "missing organization parameter"
		}
		return memberships[0].OrgID(), http.StatusOK, ""
	}
	for _, m := range memberships {
		if m.OrgID() == orgID {
			return orgID, http.StatusOK, ""
		}
	}
	return "", http.StatusNotFound, "organization not found"
}
