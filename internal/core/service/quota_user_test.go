package service_test

import (
	"context"
	"testing"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/service"
)

func TestQuotaService_ResolveUserQuota(t *testing.T) {
	orgQuota := &fakeQuota{daily: ptr[int64](6_000_000), monthly: ptr[int64](60_000_000), currency: "USD", scope: model.QuotaScopeOrg, scopeID: "org1"}
	three := []model.Membership{&fakeMembership{}, &fakeMembership{}, &fakeMembership{}}

	cases := []struct {
		name         string
		store        *fakeQuotaStore
		orgs         *fakeOrgProvider
		wantSource   model.UserQuotaSource
		wantCurrency string
		wantDaily    *int64
		wantMonthly  *int64
	}{
		{
			name:         "personal quota is not capped by the organization",
			store:        &fakeQuotaStore{orgQuota: orgQuota, userQuota: &fakeQuota{daily: ptr[int64](9_000_000), currency: "EUR", scope: model.QuotaScopeUser}},
			orgs:         &fakeOrgProvider{org: &fakeOrg{shareQuotaEqually: true}, members: three},
			wantSource:   model.UserQuotaPersonal,
			wantCurrency: "EUR",
			wantDaily:    ptr[int64](9_000_000),
		},
		{
			name:         "share of the organization budget",
			store:        &fakeQuotaStore{orgQuota: orgQuota},
			orgs:         &fakeOrgProvider{org: &fakeOrg{shareQuotaEqually: true}, members: three},
			wantSource:   model.UserQuotaShared,
			wantCurrency: "USD",
			wantDaily:    ptr[int64](2_000_000),
			wantMonthly:  ptr[int64](20_000_000),
		},
		{
			name:         "personal quota without budget is no quota",
			store:        &fakeQuotaStore{orgQuota: orgQuota, userQuota: &fakeQuota{currency: "EUR", scope: model.QuotaScopeUser}},
			orgs:         &fakeOrgProvider{org: &fakeOrg{shareQuotaEqually: true}, members: three},
			wantSource:   model.UserQuotaShared,
			wantCurrency: "USD",
			wantDaily:    ptr[int64](2_000_000),
			wantMonthly:  ptr[int64](20_000_000),
		},
		{
			name:         "organization budget not shared",
			store:        &fakeQuotaStore{orgQuota: orgQuota},
			orgs:         &fakeOrgProvider{org: &fakeOrg{}, members: three},
			wantSource:   model.UserQuotaNone,
			wantCurrency: "USD",
		},
		{
			name:         "shared organization without members",
			store:        &fakeQuotaStore{orgQuota: orgQuota},
			orgs:         &fakeOrgProvider{org: &fakeOrg{shareQuotaEqually: true}},
			wantSource:   model.UserQuotaNone,
			wantCurrency: "USD",
		},
		{
			name:         "no quota at all",
			store:        &fakeQuotaStore{},
			orgs:         &fakeOrgProvider{org: &fakeOrg{shareQuotaEqually: true}, members: three},
			wantSource:   model.UserQuotaNone,
			wantCurrency: model.DefaultCurrency,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := service.NewQuotaService(tc.store, tc.orgs).ResolveUserQuota(context.Background(), "user1", "org1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Source != tc.wantSource || got.Currency != tc.wantCurrency {
				t.Errorf("got source %q currency %q, want %q %q", got.Source, got.Currency, tc.wantSource, tc.wantCurrency)
			}
			assertBudget(t, "daily", got.DailyBudget, tc.wantDaily)
			assertBudget(t, "monthly", got.MonthlyBudget, tc.wantMonthly)
			assertBudget(t, "yearly", got.YearlyBudget, nil)
		})
	}
}

func TestQuotaService_ResolveUserQuota_MemberListFailure(t *testing.T) {
	orgQuota := &fakeQuota{daily: ptr[int64](6_000_000), currency: "EUR", scope: model.QuotaScopeOrg}
	svc := service.NewQuotaService(
		&fakeQuotaStore{orgQuota: orgQuota},
		&fakeOrgProvider{org: &fakeOrg{shareQuotaEqually: true}, listErr: context.Canceled},
	)
	if _, err := svc.ResolveUserQuota(context.Background(), "user1", "org1"); err == nil {
		t.Fatal("expected an error")
	}
}

func assertBudget(t *testing.T, period string, got, want *int64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil || *got != *want:
		t.Errorf("%s budget: got %v, want %v", period, deref(got), deref(want))
	}
}

func deref(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
