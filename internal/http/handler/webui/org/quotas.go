package org

import (
	"context"
	"fmt"
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

	existing, err := h.quotaStore.GetQuota(ctx, model.QuotaScopeOrg, string(org.ID()))
	loadError := ""
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		slog.ErrorContext(ctx, "could not load org budget", slogx.Error(err))
		loadError = "Budget indisponible : le store a renvoyé une erreur. Le formulaire ci-dessous reste éditable mais le total dépensé peut être inexact."
	}

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	dailyCost, dailyErr := h.loadOrgSpend(ctx, nil, org.ID(), startOfPeriod("day", now), orgCurrency)
	monthlyCost, monthlyErr := h.loadOrgSpend(ctx, nil, org.ID(), startOfPeriod("month", now), orgCurrency)
	yearlyCost, yearlyErr := h.loadOrgSpend(ctx, nil, org.ID(), startOfPeriod("year", now), orgCurrency)
	spendLoadError := ""
	for _, e := range []error{dailyErr, monthlyErr, yearlyErr} {
		if e != nil {
			slog.ErrorContext(ctx, "could not load org spend", slogx.Error(e))
			spendLoadError = "Consommation indisponible : le store a renvoyé une erreur. Les barres peuvent afficher 0 %."
			break
		}
	}
	switch {
	case loadError != "" && spendLoadError != "":
		loadError = loadError + " " + spendLoadError
	case spendLoadError != "":
		loadError = spendLoadError
	}

	vmodel := component.QuotaPageVModel{
		Org:         org,
		ScopeType:   "org",
		ScopeID:     string(org.ID()),
		Quota:       existing,
		Success:     r.URL.Query().Get("success"),
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		LoadError:   loadError,
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
	user := httpCtx.User(ctx)
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
	daily, monthly, yearly, fieldErrors := parseQuotaBudgetFields(r)

	if len(fieldErrors) > 0 {
		// Re-render the form rather than 400 + redirect: an operator who
		// typed a bad value needs the page back with their input and the
		// reason it was rejected, not a generic error and certainly not a
		// ?success=saved banner that lies about the outcome (issue #88).
		h.renderOrgQuotaFormError(w, r, ctx, user, orgSlug, org, fieldErrors)
		return
	}

	quota := model.NewQuota(model.QuotaScopeOrg, string(org.ID()), currency, daily, monthly, yearly)
	if err := h.quotaStore.SetQuota(ctx, quota); err != nil {
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

	existing, err := h.quotaStore.GetQuota(ctx, model.QuotaScopeUser, string(membership.UserID()))
	loadError := ""
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		slog.ErrorContext(ctx, "could not load member budget", slogx.Error(err))
		loadError = "Budget indisponible : le store a renvoyé une erreur. Le formulaire ci-dessous reste éditable mais le total dépensé peut être inexact."
	}

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	userIDs := []model.UserID{membership.UserID()}
	dailyCost, dailyErr := h.loadOrgSpend(ctx, userIDs, org.ID(), startOfPeriod("day", now), orgCurrency)
	monthlyCost, monthlyErr := h.loadOrgSpend(ctx, userIDs, org.ID(), startOfPeriod("month", now), orgCurrency)
	yearlyCost, yearlyErr := h.loadOrgSpend(ctx, userIDs, org.ID(), startOfPeriod("year", now), orgCurrency)
	spendLoadError := ""
	for _, e := range []error{dailyErr, monthlyErr, yearlyErr} {
		if e != nil {
			slog.ErrorContext(ctx, "could not load member spend", slogx.Error(e))
			spendLoadError = "Consommation indisponible : le store a renvoyé une erreur. Les barres peuvent afficher 0 %."
			break
		}
	}
	switch {
	case loadError != "" && spendLoadError != "":
		loadError = loadError + " " + spendLoadError
	case spendLoadError != "":
		loadError = spendLoadError
	}

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
		LoadError:   loadError,
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

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	currency := org.Currency()
	if currency == "" {
		currency = model.DefaultCurrency
	}
	daily, monthly, yearly, fieldErrors := parseQuotaBudgetFields(r)

	if len(fieldErrors) > 0 {
		h.renderMemberQuotaFormError(w, r, ctx, user, orgSlug, org, membership, fieldErrors)
		return
	}

	quota := model.NewQuota(model.QuotaScopeUser, string(membership.UserID()), currency, daily, monthly, yearly)
	if err := h.quotaStore.SetQuota(ctx, quota); err != nil {
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

	existing, err := h.quotaStore.GetQuota(ctx, model.QuotaScopeApplication, appID)
	loadError := ""
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		slog.ErrorContext(ctx, "could not load application budget", slogx.Error(err))
		loadError = "Budget indisponible : le store a renvoyé une erreur. Le formulaire ci-dessous reste éditable mais le total dépensé peut être inexact."
	}

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	dailyCost, dailyErr := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), model.StartOfDay(now))
	monthlyCost, monthlyErr := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), model.StartOfMonth(now))
	yearlyCost, yearlyErr := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), model.StartOfYear(now))

	// A failing spend lookup collapses the consumed figure to zero. Surface it
	// on the page so the operator does not read a misleading 0% that happens
	// to be coincidentally close to the budget. Combine with the quota-load
	// error if both are present, so the operator sees one banner that covers
	// the whole page rather than two stacked ones.
	spendLoadError := ""
	for _, e := range []error{dailyErr, monthlyErr, yearlyErr} {
		if e != nil {
			slog.ErrorContext(ctx, "could not load application spend", slogx.Error(e))
			spendLoadError = "Consommation indisponible : le store a renvoyé une erreur. Les barres peuvent afficher 0 %."
			break
		}
	}
	switch {
	case loadError != "" && spendLoadError != "":
		loadError = loadError + " " + spendLoadError
	case spendLoadError != "":
		loadError = spendLoadError
	}

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
		LoadError:   loadError,
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
	user := httpCtx.User(ctx)
	orgSlug := r.PathValue("orgSlug")
	appID := r.PathValue("appID")

	org, app, err := h.resolveOrgAndApplication(ctx, orgSlug, appID)
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
	daily, monthly, yearly, fieldErrors := parseQuotaBudgetFields(r)

	if len(fieldErrors) > 0 {
		h.renderApplicationQuotaFormError(w, r, ctx, user, orgSlug, org, app, fieldErrors)
		return
	}

	quota := model.NewQuota(model.QuotaScopeApplication, appID, currency, daily, monthly, yearly)
	if err := h.quotaStore.SetQuota(ctx, quota); err != nil {
		slog.ErrorContext(ctx, "could not save application quota", slogx.Error(err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/orgs/"+orgSlug+"/admin/applications/"+appID+"/quota?success=saved", http.StatusSeeOther)
}

// parseQuotaBudgetFields runs parseBudgetField over the three budget inputs
// of the quota editor form. The first error wins per field, so a single
// field with two issues does not produce two messages; the whole map is
// still returned so the caller can highlight every offending input at once.
//
// A nil entry on the returned map means the field either parsed cleanly or
// was empty (unlimited). An absent key means the field had no input at all,
// which is the unlimited case for the form.
func parseQuotaBudgetFields(r *http.Request) (daily, monthly, yearly *int64, fieldErrors map[string]string) {
	daily, errD := parseBudgetField(r.FormValue("daily_budget"))
	monthly, errM := parseBudgetField(r.FormValue("monthly_budget"))
	yearly, errY := parseBudgetField(r.FormValue("yearly_budget"))
	if errD != nil || errM != nil || errY != nil {
		fieldErrors = make(map[string]string)
		if errD != nil {
			fieldErrors["daily_budget"] = errD.Error()
		}
		if errM != nil {
			fieldErrors["monthly_budget"] = errM.Error()
		}
		if errY != nil {
			fieldErrors["yearly_budget"] = errY.Error()
		}
	}
	return daily, monthly, yearly, fieldErrors
}

// quotaFormSubmitted picks the raw string the operator typed out of the
// POSTed form. The form re-renders with these values rather than the stored
// quota, so a rejected value does not silently vanish and the next save can
// be a fix-up of the same input rather than a re-typing from memory.
func quotaFormSubmitted(r *http.Request) map[string]string {
	return map[string]string{
		"daily_budget":   r.FormValue("daily_budget"),
		"monthly_budget": r.FormValue("monthly_budget"),
		"yearly_budget":  r.FormValue("yearly_budget"),
	}
}

// renderOrgQuotaFormError re-renders the org quota editor when validation
// fails on POST. The page is returned with HTTP 422, the field errors
// attached to the view model and the operator's submitted values, so the
// "?success=saved" redirect is never reached on a rejected submit
// (issue #88). Spending figures are loaded the same way as on GET so the
// operator still sees their current consumption alongside the editor; any
// load error surfaces as a banner, mirroring the GET path.
func (h *Handler) renderOrgQuotaFormError(w http.ResponseWriter, r *http.Request, ctx context.Context, user model.User, orgSlug string, org model.Organization, fieldErrors map[string]string) {
	existing, err := h.quotaStore.GetQuota(ctx, model.QuotaScopeOrg, string(org.ID()))
	loadError := ""
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		slog.ErrorContext(ctx, "could not load org budget for re-render", slogx.Error(err))
		loadError = "Budget indisponible : le store a renvoyé une erreur. Le formulaire ci-dessous reste éditable mais le total dépensé peut être inexact."
	}

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	dailyCost, dailyErr := h.loadOrgSpend(ctx, nil, org.ID(), startOfPeriod("day", now), orgCurrency)
	monthlyCost, monthlyErr := h.loadOrgSpend(ctx, nil, org.ID(), startOfPeriod("month", now), orgCurrency)
	yearlyCost, yearlyErr := h.loadOrgSpend(ctx, nil, org.ID(), startOfPeriod("year", now), orgCurrency)
	for _, e := range []error{dailyErr, monthlyErr, yearlyErr} {
		if e != nil {
			slog.ErrorContext(ctx, "could not load org spend for re-render", slogx.Error(e))
			loadError = "Consommation indisponible : le store a renvoyé une erreur. Les barres peuvent afficher 0 %."
			break
		}
	}

	vmodel := component.QuotaPageVModel{
		Org:         org,
		ScopeType:   "org",
		ScopeID:     string(org.ID()),
		Quota:       existing,
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		LoadError:   loadError,
		Submitted:   quotaFormSubmitted(r),
		FieldErrors: fieldErrors,
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
	w.WriteHeader(http.StatusUnprocessableEntity)
	templ.Handler(component.QuotaPage(vmodel)).ServeHTTP(w, r)
}

// renderMemberQuotaFormError mirrors renderOrgQuotaFormError for the
// per-member quota editor. Same shape, different scope.
func (h *Handler) renderMemberQuotaFormError(w http.ResponseWriter, r *http.Request, ctx context.Context, user model.User, orgSlug string, org model.Organization, membership model.Membership, fieldErrors map[string]string) {
	existing, err := h.quotaStore.GetQuota(ctx, model.QuotaScopeUser, string(membership.UserID()))
	loadError := ""
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		slog.ErrorContext(ctx, "could not load member budget for re-render", slogx.Error(err))
		loadError = "Budget indisponible : le store a renvoyé une erreur. Le formulaire ci-dessous reste éditable mais le total dépensé peut être inexact."
	}

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	userIDs := []model.UserID{membership.UserID()}
	dailyCost, dailyErr := h.loadOrgSpend(ctx, userIDs, org.ID(), startOfPeriod("day", now), orgCurrency)
	monthlyCost, monthlyErr := h.loadOrgSpend(ctx, userIDs, org.ID(), startOfPeriod("month", now), orgCurrency)
	yearlyCost, yearlyErr := h.loadOrgSpend(ctx, userIDs, org.ID(), startOfPeriod("year", now), orgCurrency)
	for _, e := range []error{dailyErr, monthlyErr, yearlyErr} {
		if e != nil {
			slog.ErrorContext(ctx, "could not load member spend for re-render", slogx.Error(e))
			loadError = "Consommation indisponible : le store a renvoyé une erreur. Les barres peuvent afficher 0 %."
			break
		}
	}

	vmodel := component.QuotaPageVModel{
		Org:         org,
		Membership:  membership,
		ScopeType:   "user",
		ScopeID:     string(membership.UserID()),
		Quota:       existing,
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		LoadError:   loadError,
		Submitted:   quotaFormSubmitted(r),
		FieldErrors: fieldErrors,
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
	w.WriteHeader(http.StatusUnprocessableEntity)
	templ.Handler(component.QuotaPage(vmodel)).ServeHTTP(w, r)
}

// renderApplicationQuotaFormError mirrors renderOrgQuotaFormError for the
// per-application quota editor (issue #64). The application is needed to
// render the breadcrumbs and the application row of the layout.
func (h *Handler) renderApplicationQuotaFormError(w http.ResponseWriter, r *http.Request, ctx context.Context, user model.User, orgSlug string, org model.Organization, app model.Application, fieldErrors map[string]string) {
	appID := string(app.ID())
	existing, err := h.quotaStore.GetQuota(ctx, model.QuotaScopeApplication, appID)
	loadError := ""
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		slog.ErrorContext(ctx, "could not load application budget for re-render", slogx.Error(err))
		loadError = "Budget indisponible : le store a renvoyé une erreur. Le formulaire ci-dessous reste éditable mais le total dépensé peut être inexact."
	}

	orgCurrency := org.Currency()
	if orgCurrency == "" {
		orgCurrency = model.DefaultCurrency
	}
	now := time.Now()
	dailyCost, dailyErr := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), model.StartOfDay(now))
	monthlyCost, monthlyErr := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), model.StartOfMonth(now))
	yearlyCost, yearlyErr := applicationSpend(ctx, h.usageStore, model.ApplicationID(appID), org.ID(), model.StartOfYear(now))
	for _, e := range []error{dailyErr, monthlyErr, yearlyErr} {
		if e != nil {
			slog.ErrorContext(ctx, "could not load application spend for re-render", slogx.Error(e))
			loadError = "Consommation indisponible : le store a renvoyé une erreur. Les barres peuvent afficher 0 %."
			break
		}
	}

	vmodel := component.QuotaPageVModel{
		Org:         org,
		Application: app,
		ScopeType:   "application",
		ScopeID:     appID,
		Quota:       existing,
		DailyCost:   dailyCost,
		MonthlyCost: monthlyCost,
		YearlyCost:  yearlyCost,
		LoadError:   loadError,
		Submitted:   quotaFormSubmitted(r),
		FieldErrors: fieldErrors,
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
				{Label: app.Name(), Href: "/orgs/" + orgSlug + "/admin/applications/" + appID + "/edit"},
				{Label: "Budget", Href: ""},
			},
		},
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	templ.Handler(component.QuotaPage(vmodel)).ServeHTTP(w, r)
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
//
// The request context is threaded through so a cancellation propagates to the
// store call; dropping it (context.Background) would let a cancelled client
// still pay the cost of the counter read. A non-nil error is logged and
// returned to the caller so it can surface a "Budget indisponible" banner
// alongside the zero figure — a silent 0% would otherwise hide a real store
// failure.
func applicationSpend(ctx context.Context, store port.UsageStore, appID model.ApplicationID, orgID model.OrgID, since time.Time) (int64, error) {
	total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeApplication, string(appID), orgID, since)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// maxBudgetMicrocents is the largest microcent amount parseBudgetField will
// emit. It is math.MaxInt64 rounded down to the nearest whole unit (1 unit ==
// 1 000 000 microcents) so the int64 conversion below cannot overflow: the
// ticket's "1e16" example would silently become a small positive value without
// this clamp, because int64(1e16 * 1_000_000) wraps to a negative number that
// then rounds back into a positive one through the existing abs-style usage.
const maxBudgetMicrocents = math.MaxInt64 / 1_000_000

// parseBudgetField parses a currency budget field into microcents.
//
// Return values:
//
//   - Empty → (nil, nil): unlimited, the historical default.
//
//   - "0" → (&0, nil): strict zero cap. The enforcer compares spent >= budget
//     and spent is always >= 0, so every PAYG request gets a 429. This is the
//     documented way to freeze an application or member immediately, and the
//     reason the field accepts 0 in the first place: the MoneyInput renders
//     with min="0", so removing that value would also reject the legitimate
//     freeze case.
//
//   - Anything else → (*int64, nil) on success, (nil, error) on a value that
//     cannot be trusted. The caller is expected to surface the error on the
//     form rather than silently store a budget of 0, which is what the
//     previous parser did and what issue #88 is the fix for.
//
//   - Negative values, NaN, ±Inf and unparsable strings are rejected. A value
//     larger than maxBudgetMicrocents (math.MaxInt64 / 1_000_000, in whole
//     units) is also rejected with a clear message — without the cap, a
//     hand-crafted "1e16" silently overflowed int64 during the * 1_000_000
//     conversion and stored an arbitrary amount, which is worse than rejecting
//     the input outright.
func parseBudgetField(v string) (*int64, error) {
	if v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, fmt.Errorf("valeur invalide %q : nombre attendu (ex. 10.50)", v)
	}
	if math.IsNaN(f) {
		return nil, errors.New("valeur invalide : NaN n'est pas un plafond acceptable")
	}
	if math.IsInf(f, 0) {
		return nil, errors.New("valeur invalide : un nombre infini n'est pas un plafond acceptable")
	}
	if f < 0 {
		return nil, fmt.Errorf("valeur invalide %q : le plafond doit être positif ou nul", v)
	}
	if f > float64(maxBudgetMicrocents) {
		return nil, fmt.Errorf("valeur invalide %q : le plafond dépasse le maximum supporté (%d)", v, maxBudgetMicrocents)
	}
	mc := int64(f * 1_000_000)
	return &mc, nil
}
