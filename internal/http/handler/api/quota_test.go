package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xolo-gateway/xolo/internal/core/model"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/handler/api"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn"
)

type fakeUserQuotas struct {
	quota  *model.UserQuota
	err    error
	gotOrg model.OrgID
}

func (f *fakeUserQuotas) ResolveUserQuota(_ context.Context, _ model.UserID, orgID model.OrgID) (*model.UserQuota, error) {
	f.gotOrg = orgID
	return f.quota, f.err
}

type quotaSpendCall struct {
	scope   model.QuotaScope
	scopeID string
	orgID   model.OrgID
	since   time.Time
}

type fakeQuotaSpend struct {
	spent map[time.Time]int64
	err   error
	calls []quotaSpendCall
}

func (f *fakeQuotaSpend) SumQuotaCostSince(_ context.Context, scope model.QuotaScope, scopeID string, orgID model.OrgID, since time.Time) (int64, error) {
	f.calls = append(f.calls, quotaSpendCall{scope, scopeID, orgID, since})
	return f.spent[since], f.err
}

type quotaTestCase struct {
	user        model.User
	tokenOrgID  string
	memberships []model.OrgID
	// contextMemberships, when set, are stored in the request context as the
	// memberships middleware does.
	contextMemberships []model.OrgID
	query              string
}

func serveQuota(t *testing.T, tc quotaTestCase, quotas *fakeUserQuotas, spend *fakeQuotaSpend, now time.Time) *httptest.ResponseRecorder {
	t.Helper()
	memberships := map[string][]model.Membership{}
	if tc.user != nil {
		for _, orgID := range tc.memberships {
			memberships[string(tc.user.ID())] = append(memberships[string(tc.user.ID())], model.NewMembership(tc.user.ID(), orgID))
		}
	}
	orgStore := &fakeOrgStoreForModels{memberships: memberships}
	h := api.NewHandler(nil, orgStore, nil, nil, nil, nil, nil, nil).
		WithUserQuota(quotas, spend, func() time.Time { return now })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/quota"+tc.query, nil)
	if tc.user != nil {
		ctx := authn.SetContextUser(req.Context(), &authn.User{
			Provider: tc.user.Provider(),
			Subject:  tc.user.Subject(),
			OrgID:    tc.tokenOrgID,
			TenantID: string(testTenantID),
		})
		ctx = httpCtx.SetUser(ctx, tc.user)
		if tc.contextMemberships != nil {
			fromContext := []model.Membership{}
			for _, orgID := range tc.contextMemberships {
				fromContext = append(fromContext, model.NewMembership(tc.user.ID(), orgID))
			}
			ctx = httpCtx.SetMemberships(ctx, fromContext)
		}
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func quotaUser() model.User {
	return model.NewUser(testTenantID, "oidc", "human-1", "alice@example.com", "Alice", true, "user")
}

func TestHandleQuota_ReportsUserLevel(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 15, 4, 0, 0, paris)
	day := time.Date(2026, 10, 13, 0, 0, 0, 0, paris)
	month := time.Date(2026, 10, 1, 0, 0, 0, 0, paris)
	year := time.Date(2026, 1, 1, 0, 0, 0, 0, paris)

	user := quotaUser()
	quotas := &fakeUserQuotas{quota: &model.UserQuota{
		Source:        model.UserQuotaPersonal,
		Currency:      "EUR",
		DailyBudget:   ptr[int64](1_000_000),
		MonthlyBudget: ptr[int64](20_000_000),
	}}
	spend := &fakeQuotaSpend{spent: map[time.Time]int64{day: 1_500_000, month: 4_000_000, year: 9_000_000}}

	rec := serveQuota(t, quotaTestCase{user: user, memberships: []model.OrgID{"org-a", "org-b"}, query: "?organization=org-b"}, quotas, spend, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if quotas.gotOrg != "org-b" {
		t.Errorf("resolved quota of %q, want org-b", quotas.gotOrg)
	}
	for _, call := range spend.calls {
		if call.scope != model.QuotaScopeUser || call.scopeID != string(user.ID()) || call.orgID != "org-b" {
			t.Errorf("unexpected spend lookup %+v", call)
		}
	}

	var body struct {
		OrganizationID string `json:"organization_id"`
		Source         string `json:"source"`
		Currency       string `json:"currency"`
		Periods        map[string]struct {
			Budget    *int64    `json:"budget"`
			Spent     int64     `json:"spent"`
			Remaining *int64    `json:"remaining"`
			ResetsAt  time.Time `json:"resets_at"`
		} `json:"periods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OrganizationID != "org-b" || body.Source != "personal" || body.Currency != "EUR" {
		t.Errorf("unexpected header fields %+v", body)
	}

	daily := body.Periods["daily"]
	if *daily.Budget != 1_000_000 || daily.Spent != 1_500_000 || *daily.Remaining != 0 || !daily.ResetsAt.Equal(day.AddDate(0, 0, 1)) {
		t.Errorf("daily: budget %v spent %d remaining %v resets %v", *daily.Budget, daily.Spent, *daily.Remaining, daily.ResetsAt)
	}
	monthly := body.Periods["monthly"]
	if *monthly.Budget != 20_000_000 || monthly.Spent != 4_000_000 || *monthly.Remaining != 16_000_000 || !monthly.ResetsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, paris)) {
		t.Errorf("monthly: budget %v spent %d remaining %v resets %v", *monthly.Budget, monthly.Spent, *monthly.Remaining, monthly.ResetsAt)
	}
	yearly := body.Periods["yearly"]
	if yearly.Budget != nil || yearly.Remaining != nil || yearly.Spent != 9_000_000 || !yearly.ResetsAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, paris)) {
		t.Errorf("yearly: budget %v spent %d remaining %v resets %v", yearly.Budget, yearly.Spent, yearly.Remaining, yearly.ResetsAt)
	}
}

func TestHandleQuota_ResolvesOrganization(t *testing.T) {
	user := quotaUser()
	app := model.NewUser(testTenantID, model.ApplicationProvider, "app-1", "", "CI", true, "user")

	cases := []struct {
		name       string
		tc         quotaTestCase
		wantStatus int
		wantOrg    model.OrgID
	}{
		{"only membership", quotaTestCase{user: user, memberships: []model.OrgID{"org-a"}}, http.StatusOK, "org-a"},
		{"several memberships without parameter", quotaTestCase{user: user, memberships: []model.OrgID{"org-a", "org-b"}}, http.StatusBadRequest, ""},
		{"no membership", quotaTestCase{user: user}, http.StatusBadRequest, ""},
		{"not a member", quotaTestCase{user: user, memberships: []model.OrgID{"org-a"}, query: "?organization=org-b"}, http.StatusNotFound, ""},
		{"token organization", quotaTestCase{user: user, tokenOrgID: "org-b", memberships: []model.OrgID{"org-a", "org-b"}}, http.StatusOK, "org-b"},
		{"token bound to another organization", quotaTestCase{user: user, tokenOrgID: "org-b", memberships: []model.OrgID{"org-a", "org-b"}, query: "?organization=org-a"}, http.StatusNotFound, ""},
		{"token organization no longer a membership", quotaTestCase{user: user, tokenOrgID: "org-b", memberships: []model.OrgID{"org-a"}}, http.StatusNotFound, ""},
		// The store disagrees with the context: the context must win.
		{"memberships from the context", quotaTestCase{user: user, memberships: []model.OrgID{"org-a"}, contextMemberships: []model.OrgID{"org-b"}}, http.StatusOK, "org-b"},
		{"application", quotaTestCase{user: app, tokenOrgID: "org-a"}, http.StatusForbidden, ""},
		{"unauthenticated", quotaTestCase{}, http.StatusUnauthorized, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			quotas := &fakeUserQuotas{quota: &model.UserQuota{Source: model.UserQuotaNone, Currency: "EUR"}}
			rec := serveQuota(t, c.tc, quotas, &fakeQuotaSpend{}, time.Now())
			if rec.Code != c.wantStatus {
				t.Fatalf("expected %d, got %d: %s", c.wantStatus, rec.Code, rec.Body.String())
			}
			if quotas.gotOrg != c.wantOrg {
				t.Errorf("resolved quota of %q, want %q", quotas.gotOrg, c.wantOrg)
			}
		})
	}
}

func TestHandleQuota_StoreFailures(t *testing.T) {
	tc := quotaTestCase{user: quotaUser(), memberships: []model.OrgID{"org-a"}}
	cases := map[string]struct {
		quotas *fakeUserQuotas
		spend  *fakeQuotaSpend
	}{
		"quota": {&fakeUserQuotas{err: errors.New("boom")}, &fakeQuotaSpend{}},
		"spend": {&fakeUserQuotas{quota: &model.UserQuota{Source: model.UserQuotaNone}}, &fakeQuotaSpend{err: errors.New("boom")}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serveQuota(t, tc, c.quotas, c.spend, time.Now())
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
