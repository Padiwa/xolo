package org

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	common "github.com/xolo-gateway/xolo/internal/http/handler/webui/common/component"
	"github.com/xolo-gateway/xolo/internal/http/handler/webui/org/component"
)

func (h *Handler) getOrgQuotaPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := httpCtx.User(ctx)
	orgSlug := r.PathValue("orgSlug")

	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		http.Error(w, "Organization not found", http.StatusNotFound)
		return
	}

	quotaStore := h.quotaStore

	existing, _ := quotaStore.GetQuota(ctx, model.QuotaScopeOrg, string(org.ID()))

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	dailyCost := h.sumConvertedCost(ctx, nil, org.ID(), startOfPeriod("day", now), orgCurrency)
	monthlyCost := h.sumConvertedCost(ctx, nil, org.ID(), startOfPeriod("month", now), orgCurrency)
	yearlyCost := h.sumConvertedCost(ctx, nil, org.ID(), startOfPeriod("year", now), orgCurrency)

	vmodel := component.QuotaPageVModel{
		Org:         org,
		ScopeType:   "org",
		ScopeID:     string(org.ID()),
		Quota:       existing,
		Success:     r.URL.Query().Get("success"),
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		AppLayoutVModel: common.AppLayoutVModel{
			User:         user,
			SelectedItem: "org-" + orgSlug + "-quota",
			Context:      common.ContextOrg,
			ContextName:  org.Name(),
			ContextSlug:  org.Slug(),
			ContextOrgID: org.ID(),
			Breadcrumbs: []common.BreadcrumbItem{
				{Label: org.Name(), Href: "/orgs/" + orgSlug + "/usage"},
				{Label: "Budget", Href: "/orgs/" + orgSlug + "/admin/quota"},
			},
		},
	}

	templ.Handler(component.QuotaPage(vmodel)).ServeHTTP(w, r)
}

func (h *Handler) saveOrgQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgSlug := r.PathValue("orgSlug")

	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		http.Error(w, "Organization not found", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	currency := org.Currency()
	if currency == "" {
		currency = model.DefaultCurrency
	}
	daily := parseBudgetField(r.FormValue("daily_budget"))
	monthly := parseBudgetField(r.FormValue("monthly_budget"))
	yearly := parseBudgetField(r.FormValue("yearly_budget"))

	quotaStore := h.quotaStore

	quota := model.NewQuota(model.QuotaScopeOrg, string(org.ID()), currency, daily, monthly, yearly)
	if err := quotaStore.SetQuota(ctx, quota); err != nil {
		slog.ErrorContext(ctx, "could not save org quota", slogx.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/orgs/"+orgSlug+"/admin/quota?success=saved", http.StatusSeeOther)
}

func (h *Handler) getMemberQuotaPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := httpCtx.User(ctx)
	orgSlug := r.PathValue("orgSlug")
	membershipID := r.PathValue("membershipID")

	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		http.Error(w, "Organization not found", http.StatusNotFound)
		return
	}

	membership, err := h.orgStore.GetMembership(ctx, model.MembershipID(membershipID))
	if err != nil {
		if errors.Is(err, port.ErrNotFound) {
			http.Error(w, "Membership not found", http.StatusNotFound)
			return
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	quotaStore := h.quotaStore

	existing, _ := quotaStore.GetQuota(ctx, model.QuotaScopeUser, string(membership.UserID()))

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	userIDs := []model.UserID{membership.UserID()}
	dailyCost := h.sumConvertedCost(ctx, userIDs, org.ID(), startOfPeriod("day", now), orgCurrency)
	monthlyCost := h.sumConvertedCost(ctx, userIDs, org.ID(), startOfPeriod("month", now), orgCurrency)
	yearlyCost := h.sumConvertedCost(ctx, userIDs, org.ID(), startOfPeriod("year", now), orgCurrency)

	vmodel := component.QuotaPageVModel{
		Org:         org,
		Membership:  membership,
		ScopeType:   "user",
		ScopeID:     string(membership.UserID()),
		Quota:       existing,
		Success:     r.URL.Query().Get("success"),
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		AppLayoutVModel: common.AppLayoutVModel{
			User:         user,
			SelectedItem: "org-" + orgSlug + "-members",
			Context:      common.ContextOrg,
			ContextName:  org.Name(),
			ContextSlug:  org.Slug(),
			ContextOrgID: org.ID(),
			Breadcrumbs: []common.BreadcrumbItem{
				{Label: org.Name(), Href: "/orgs/" + orgSlug + "/usage"},
				{Label: "Membres", Href: "/orgs/" + orgSlug + "/admin/members"},
				{Label: membership.User().DisplayName(), Href: ""},
				{Label: "Budget", Href: ""},
			},
		},
	}

	templ.Handler(component.QuotaPage(vmodel)).ServeHTTP(w, r)
}

func (h *Handler) saveMemberQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgSlug := r.PathValue("orgSlug")
	membershipID := r.PathValue("membershipID")

	org, err := h.orgFromSlug(ctx, orgSlug)
	if err != nil {
		http.Error(w, "Organization not found", http.StatusNotFound)
		return
	}

	membership, err := h.orgStore.GetMembership(ctx, model.MembershipID(membershipID))
	if err != nil {
		if errors.Is(err, port.ErrNotFound) {
			http.Error(w, "Membership not found", http.StatusNotFound)
			return
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	currency := org.Currency()
	if currency == "" {
		currency = model.DefaultCurrency
	}
	daily := parseBudgetField(r.FormValue("daily_budget"))
	monthly := parseBudgetField(r.FormValue("monthly_budget"))
	yearly := parseBudgetField(r.FormValue("yearly_budget"))

	quotaStore := h.quotaStore

	quota := model.NewQuota(model.QuotaScopeUser, string(membership.UserID()), currency, daily, monthly, yearly)
	if err := quotaStore.SetQuota(ctx, quota); err != nil {
		slog.ErrorContext(ctx, "could not save member quota", slogx.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/orgs/"+orgSlug+"/admin/members/"+membershipID+"/quota?success=saved", http.StatusSeeOther)
}

// getApplicationQuotaPage renders the budget editor for one application. The
// view model is the same component as the org and member quota pages, just
// scoped to QuotaScopeApplication (issue #64): an application token can spend
// unattended, so the operator needs a way to cap its daily/monthly/yearly
// spend through the product rather than via a direct DB write or the
// provisioning API.
func (h *Handler) getApplicationQuotaPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := httpCtx.User(ctx)
	orgSlug := r.PathValue("orgSlug")
	appID := r.PathValue("appID")

	org, app, err := h.resolveOrgAndApplication(ctx, orgSlug, appID)
	if err != nil {
		writeApplicationLookupError(ctx, w, err)
		return
	}

	existing, _ := h.quotaStore.GetQuota(ctx, model.QuotaScopeApplication, appID)

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	dailyCost := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), orgCurrency, model.StartOfDay(now))
	monthlyCost := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), orgCurrency, model.StartOfMonth(now))
	yearlyCost := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), orgCurrency, model.StartOfYear(now))

	vmodel := component.QuotaPageVModel{
		Org:         org,
		Application: app,
		ScopeType:   "application",
		ScopeID:     string(appID),
		Quota:       existing,
		Success:     r.URL.Query().Get("success"),
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		AppLayoutVModel: common.AppLayoutVModel{
			User:         user,
			SelectedItem: "org-" + orgSlug + "-applications",
			Context:      common.ContextOrg,
			ContextName:  org.Name(),
			ContextSlug:  org.Slug(),
			ContextOrgID: org.ID(),
			Breadcrumbs: []common.BreadcrumbItem{
				{Label: org.Name(), Href: "/orgs/" + orgSlug + "/usage"},
				{Label: "Applications", Href: "/orgs/" + orgSlug + "/admin/applications"},
				{Label: app.Name(), Href: "/orgs/" + orgSlug + "/admin/applications/" + string(appID) + "/edit"},
				{Label: "Budget", Href: ""},
			},
		},
	}

	templ.Handler(component.QuotaPage(vmodel)).ServeHTTP(w, r)
}

// saveApplicationQuota writes a QuotaScopeApplication row for the resolved
// application. The application is loaded again on POST so an operator cannot
// edit a budget on an application outside their organization by hand-crafting
// the form action.
func (h *Handler) saveApplicationQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgSlug := r.PathValue("orgSlug")
	appID := r.PathValue("appID")

	org, _, err := h.resolveOrgAndApplication(ctx, orgSlug, appID)
	if err != nil {
		writeApplicationLookupError(ctx, w, err)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	currency := org.Currency()
	if currency == "" {
		currency = model.DefaultCurrency
	}
	daily := parseBudgetField(r.FormValue("daily_budget"))
	monthly := parseBudgetField(r.FormValue("monthly_budget"))
	yearly := parseBudgetField(r.FormValue("yearly_budget"))

	quota := model.NewQuota(model.QuotaScopeApplication, appID, currency, daily, monthly, yearly)
	if err := h.quotaStore.SetQuota(ctx, quota); err != nil {
		slog.ErrorContext(ctx, "could not save application quota", slogx.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/orgs/"+orgSlug+"/admin/applications/"+appID+"/quota?success=saved", http.StatusSeeOther)
}

// applicationSpend returns the PAYG spending attributed to one application
// since the given time, in microcents of the org's base currency. It reuses
// the same counter the enforcer reads against (QuotaScopeApplication), so the
// figure shown to the operator matches the budget check exactly: a 429 from
// the enforcer and a 100% reading on the page both describe the same total.
//
// Costs are stored already converted to the org's currency at record time, so
// the counter is a flat microcent total: no per-currency grouping is needed
// here. This is the application's slice of org spending; cross-application
// rolls up at the org-wide scope.
func applicationSpend(_ context.Context, store port.UsageStore, appID model.ApplicationID, orgID model.OrgID, _ string, since time.Time) int64 {
	sinceFn, ok := store.(interface {
		SumQuotaCostSince(ctx context.Context, scope model.QuotaScope, scopeID string, orgID model.OrgID, since time.Time) (int64, error)
	})
	if !ok {
		return 0
	}
	total, err := sinceFn.SumQuotaCostSince(context.Background(), model.QuotaScopeApplication, string(appID), orgID, since)
	if err != nil {
		return 0
	}
	return total
}

// parseBudgetField parses a currency budget field into microcents. Empty → nil (unlimited).
func parseBudgetField(v string) *int64 {
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	// NaN and ±Inf parse fine and slip past "f <= 0"; int64(NaN * 1e6) is then
	// an arbitrary amount, not an error.
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return nil
	}
	mc := int64(f * 1_000_000)
	return &mc
}
