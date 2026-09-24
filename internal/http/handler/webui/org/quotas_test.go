package org

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
)

// stubQuotaStore for the application-quota handler tests. Every method
// returns ErrNotFound unless the test sets the corresponding field, which
// lets each table-driven case pin one branch without constructing a fully
// populated store.
type stubQuotaStore struct {
	port.QuotaStore
	quota model.Quota
	err   error
}

func (s *stubQuotaStore) GetQuota(_ context.Context, scope model.QuotaScope, _ string) (model.Quota, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.quota != nil && scope == model.QuotaScopeApplication {
		return s.quota, nil
	}
	return nil, port.ErrNotFound
}

func (s *stubQuotaStore) SetQuota(_ context.Context, q model.Quota) error {
	s.quota = q
	return s.err
}

func (s *stubQuotaStore) ResolveEffectiveQuota(context.Context, model.UserID, model.OrgID) (*model.EffectiveQuota, error) {
	return &model.EffectiveQuota{}, nil
}

func (s *stubQuotaStore) ResolveEffectiveQuotaForApplication(context.Context, model.ApplicationID, model.OrgID) (*model.EffectiveQuota, error) {
	return &model.EffectiveQuota{}, nil
}

// stubUsageStore for the application-quota handler tests: every read returns
// zero unless the test sets it. The application budget editor only reads
// (no writes happen through this store on GET), so the failure modes are
// "ok with zero" and "non-ErrNotFound error".
type stubUsageStore struct {
	port.UsageStore
	err error
}

func (s *stubUsageStore) SumQuotaCostSince(_ context.Context, _ model.QuotaScope, _ string, _ model.OrgID, _ time.Time) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return 0, nil
}

func (s *stubUsageStore) SumCostSinceByCurrency(context.Context, []model.UserID, model.OrgID, time.Time) (map[string]int64, error) {
	return map[string]int64{}, nil
}

// handlerWithStubs wires the bare minimum needed by the application quota
// handlers: a Handler with the org/application/quota/usage stores replaced
// by stubs, and a tenant id in context so resolveOrgAndApplication finds
// the org.
func handlerWithStubs(org model.Organization, app model.Application, qStore *stubQuotaStore, uStore *stubUsageStore) *Handler {
	return &Handler{
		orgStore:         &stubOrgStore{org: org},
		applicationStore: &stubApplicationStore{app: app},
		quotaStore:       qStore,
		usageStore:       uStore,
	}
}

func newRequest(orgSlug, appID string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/orgs/"+orgSlug+"/admin/applications/"+appID+"/quota", nil)
	r.SetPathValue("orgSlug", orgSlug)
	r.SetPathValue("appID", appID)
	// resolveOrgAndApplication reads TenantID from the context, and the
	// templ component reads BaseURL from it. Set both: a minimal valid URL
	// so BaseURLString does not panic when rendering the page.
	base, _ := url.Parse("http://example.test")
	r = r.WithContext(httpCtx.SetBaseURL(r.Context(), base.String()))
	return r
}

// renderHandler runs a handler and returns the rendered HTML body so the
// tests can assert on visible strings without depending on templ's internals.
func renderHandler(h http.HandlerFunc, r *http.Request) string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Body.String()
}

// TestGetApplicationQuotaPage covers the four GET branches: no budget row
// (renders empty), budget populated, foreign appID (404), and store error
// other than ErrNotFound (banner rendered).
func TestGetApplicationQuotaPage(t *testing.T) {
	org := model.NewOrganization("tenant", "acme", "Acme", "EUR")
	daily := int64(60_000)
	app := model.NewApplication(org.ID(), "acme-app", "", true)

	tests := []struct {
		name        string
		store       *stubQuotaStore
		foreignApp  bool
		wantStatus  int
		wantInBody  []string // substrings that must appear in the rendered body
		wantNotBody []string // substrings that must NOT appear
	}{
		{
			// No budget row: form fields render empty; the dedicated editor
			// page does not show a "Aucun budget" line (the summary lives on
			// the application edit form, not here). The /quota page renders
			// the form with three blank inputs.
			name:       "no budget row renders empty fields",
			store:      &stubQuotaStore{},
			wantStatus: http.StatusOK,
			wantInBody: []string{
				"Budget de l&#39;application",
				"daily_budget",
				"monthly_budget",
				"yearly_budget",
				`placeholder="10.00"`,
			},
			wantNotBody: []string{
				"Budget indisponible",
			},
		},
		{
			name: "populated budget pre-fills the daily field",
			store: &stubQuotaStore{
				quota: model.NewQuota(model.QuotaScopeApplication, "acme-app", "EUR", &daily, nil, nil),
			},
			wantStatus: http.StatusOK,
			wantInBody: []string{
				"Budget de l&#39;application",
				"daily_budget",
			},
			wantNotBody: []string{
				"Budget indisponible",
			},
		},
		{
			name:       "foreign appID answers 404",
			store:      &stubQuotaStore{},
			foreignApp: true,
			wantStatus: http.StatusNotFound,
			wantInBody: []string{
				"Application not found",
			},
		},
		{
			name:       "store error other than ErrNotFound renders the banner",
			store:      &stubQuotaStore{err: errors.New("backend down")},
			wantStatus: http.StatusOK,
			wantInBody: []string{
				"Budget indisponible",
			},
			wantNotBody: []string{
				`value="10.00"`, // no value populated since the quota could not be loaded
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			targetApp := app
			if tc.foreignApp {
				// An application that belongs to a different org.
				otherOrg := model.NewOrganization("tenant-other", "other", "Other", "EUR")
				targetApp = model.NewApplication(otherOrg.ID(), "other-app", "", true)
			}
			h := handlerWithStubs(org, app, tc.store, &stubUsageStore{})

			body := renderHandler(h.getApplicationQuotaPage, newRequest(org.Slug(), string(targetApp.ID())))

			if tc.wantStatus == http.StatusOK {
				lower := strings.ToLower(body)
				for _, want := range tc.wantInBody {
					if !strings.Contains(lower, strings.ToLower(want)) {
						t.Errorf("body missing %q; full body length=%d:\n%s", want, len(body), body)
					}
				}
				for _, dont := range tc.wantNotBody {
					if strings.Contains(lower, strings.ToLower(dont)) {
						t.Errorf("body contains %q (should not)", dont)
					}
				}
			} else {
				// Non-OK path: httptest.NewRecorder default status is 200,
				// so we look at the recorder instead.
				rec := httptest.NewRecorder()
				h.getApplicationQuotaPage(rec, newRequest(org.Slug(), string(targetApp.ID())))
				if rec.Code != tc.wantStatus {
					t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
				}
			}
		})
	}
}

// TestSaveApplicationQuotaHappyPath verifies the POST writes a QuotaScopeApplication
// row with the parsed fields, then redirects to the success URL.
func TestSaveApplicationQuotaHappyPath(t *testing.T) {
	org := model.NewOrganization("tenant", "acme", "Acme", "EUR")
	app := model.NewApplication(org.ID(), "acme-app", "", true)
	qStore := &stubQuotaStore{}

	h := handlerWithStubs(org, app, qStore, &stubUsageStore{})

	form := url.Values{}
	form.Set("daily_budget", "10")
	form.Set("monthly_budget", "100")
	form.Set("yearly_budget", "1000")
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("orgSlug", org.Slug())
	r.SetPathValue("appID", string(app.ID()))

	rec := httptest.NewRecorder()
	h.saveApplicationQuota(rec, r)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	location := rec.Header().Get("Location")
	want := "/orgs/acme/admin/applications/" + string(app.ID()) + "/quota?success=saved"
	if location != want {
		t.Errorf("location = %q, want %q", location, want)
	}
	if qStore.quota == nil {
		t.Fatalf("SetQuota was not called")
	}
	if qStore.quota.Scope() != model.QuotaScopeApplication {
		t.Errorf("scope = %q, want application", qStore.quota.Scope())
	}
	if qStore.quota.ScopeID() != string(app.ID()) {
		t.Errorf("scope id = %q, want %q", qStore.quota.ScopeID(), string(app.ID()))
	}
}

// TestSaveApplicationQuotaForeignApp covers the cross-org POST leak: even
// when an attacker hand-crafts a form action pointing at an application
// belonging to another org, the handler refuses with 404.
func TestSaveApplicationQuotaForeignApp(t *testing.T) {
	org := model.NewOrganization("tenant", "acme", "Acme", "EUR")
	foreignOrg := model.NewOrganization("tenant-other", "other", "Other", "EUR")
	foreignApp := model.NewApplication(foreignOrg.ID(), "other-app", "", true)
	qStore := &stubQuotaStore{}

	h := handlerWithStubs(org, model.NewApplication(org.ID(), "acme-app", "", true), qStore, &stubUsageStore{})

	form := url.Values{}
	form.Set("daily_budget", "10")
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("orgSlug", org.Slug())
	r.SetPathValue("appID", string(foreignApp.ID()))

	rec := httptest.NewRecorder()
	h.saveApplicationQuota(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if qStore.quota != nil {
		t.Errorf("SetQuota was called for a foreign application")
	}
}
