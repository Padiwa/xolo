package service

import (
	"context"

	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

// OrgProvider is a narrow interface used by QuotaService.
// It is satisfied by port.OrgStore (and any fake with just these two methods).
type OrgProvider interface {
	GetOrgByID(ctx context.Context, id model.OrgID) (model.Organization, error)
	ListOrgMembers(ctx context.Context, orgID model.OrgID, opts port.ListOrgMembersOptions) ([]model.Membership, int64, error)
}

type QuotaService struct {
	quotaStore port.QuotaStore
	orgStore   OrgProvider
}

func NewQuotaService(quotaStore port.QuotaStore, orgStore OrgProvider) *QuotaService {
	return &QuotaService{quotaStore: quotaStore, orgStore: orgStore}
}

func (s *QuotaService) ResolveEffectiveQuota(
	ctx context.Context,
	userID model.UserID,
	orgID model.OrgID,
) (*model.EffectiveQuota, error) {
	userQuota, err := s.quotaStore.GetQuota(ctx, model.QuotaScopeUser, string(userID))
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return nil, errors.WithStack(err)
	}
	if errors.Is(err, port.ErrNotFound) {
		userQuota = nil
	}

	orgQuota, err := s.quotaStore.GetQuota(ctx, model.QuotaScopeOrg, string(orgID))
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return nil, errors.WithStack(err)
	}
	if errors.Is(err, port.ErrNotFound) {
		orgQuota = nil
	}

	org, err := s.orgStore.GetOrgByID(ctx, orgID)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	effective := &model.EffectiveQuota{}

	// A quota record with all-nil budgets is treated as "no personal quota".
	userHasPersonalQuota := userQuota != nil &&
		(userQuota.DailyBudget() != nil || userQuota.MonthlyBudget() != nil || userQuota.YearlyBudget() != nil)

	// Currency: org quota > user quota > default.
	switch {
	case orgQuota != nil:
		effective.Currency = orgQuota.Currency()
	case userHasPersonalQuota:
		effective.Currency = userQuota.Currency()
	default:
		effective.Currency = model.DefaultCurrency
	}

	// Sharing: distribute org quota equally only when user has no personal quota.
	if !userHasPersonalQuota && org.ShareQuotaEqually() && orgQuota != nil {
		members, _, err := s.orgStore.ListOrgMembers(ctx, orgID, port.ListOrgMembersOptions{})
		if err != nil {
			return nil, errors.WithStack(err)
		}
		n := int64(len(members))
		if n > 0 {
			// Synthesise a per-user quota by integer division (floor truncation).
			effective.DailyBudget = divPtr(orgQuota.DailyBudget(), n)
			effective.MonthlyBudget = divPtr(orgQuota.MonthlyBudget(), n)
			effective.YearlyBudget = divPtr(orgQuota.YearlyBudget(), n)
			return effective, nil
		}
		// n == 0: unlimited (theoretically impossible if requester is a member).
		return effective, nil
	}

	// Default: min-merge of user and org quotas.
	effective.DailyBudget = minPtrSvc(ptrOf(userQuota, func(q model.Quota) *int64 { return q.DailyBudget() }),
		ptrOf(orgQuota, func(q model.Quota) *int64 { return q.DailyBudget() }))
	effective.MonthlyBudget = minPtrSvc(ptrOf(userQuota, func(q model.Quota) *int64 { return q.MonthlyBudget() }),
		ptrOf(orgQuota, func(q model.Quota) *int64 { return q.MonthlyBudget() }))
	effective.YearlyBudget = minPtrSvc(ptrOf(userQuota, func(q model.Quota) *int64 { return q.YearlyBudget() }),
		ptrOf(orgQuota, func(q model.Quota) *int64 { return q.YearlyBudget() }))

	return effective, nil
}

func divPtr(v *int64, n int64) *int64 {
	if v == nil {
		return nil
	}
	r := *v / n
	return &r
}

// ResolveEffectiveQuotaForApplication merges the application and org quotas at
// each period, taking the minimum of the two non-nil values. Currency is taken
// from the org quota first so the same defaults ResolveEffectiveQuota uses
// apply here.
//
// There is no "sharing" branch on this path: an application has no membership
// to share an org budget across, and an operator who set a budget on the
// application means the whole of it, not a slice.
//
// The result is fed to the enforcer's application-scope check, which runs
// against the QuotaScopeApplication counter. The shadow user's user-scope
// check is not run at all on the application path: the enforcer skips it
// (see XoloQuotaEnforcer.PreRequest) because the counter is never fed for an
// application record (see quotaUsageRows). The application budget is the only
// budget the enforcer ever checks for a request carrying an ApplicationID
// (issue #64).
func (s *QuotaService) ResolveEffectiveQuotaForApplication(
	ctx context.Context,
	appID model.ApplicationID,
	orgID model.OrgID,
) (*model.EffectiveQuota, error) {
	appQuota, err := s.quotaStore.GetQuota(ctx, model.QuotaScopeApplication, string(appID))
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return nil, errors.WithStack(err)
	}
	if errors.Is(err, port.ErrNotFound) {
		appQuota = nil
	}

	orgQuota, err := s.quotaStore.GetQuota(ctx, model.QuotaScopeOrg, string(orgID))
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return nil, errors.WithStack(err)
	}
	if errors.Is(err, port.ErrNotFound) {
		orgQuota = nil
	}

	effective := &model.EffectiveQuota{}
	// Currency precedence: org quota first, then app quota, then default.
	// Both quota writers freeze budget amounts in the org's currency at
	// SetQuota time (the handler does the conversion), so even if an operator
	// somehow wrote a QuotaScopeApplication row in a different currency, its
	// microcent amounts are still in org currency by the time we min-merge
	// them. Picking the org currency here keeps the merged EffectiveQuota
	// internally consistent: every *int64 it returns is denominated in the
	// same currency.
	switch {
	case orgQuota != nil:
		effective.Currency = orgQuota.Currency()
	case appQuota != nil:
		effective.Currency = appQuota.Currency()
	default:
		effective.Currency = model.DefaultCurrency
	}

	effective.DailyBudget = minPtrSvc(
		ptrOf(appQuota, func(q model.Quota) *int64 { return q.DailyBudget() }),
		ptrOf(orgQuota, func(q model.Quota) *int64 { return q.DailyBudget() }),
	)
	effective.MonthlyBudget = minPtrSvc(
		ptrOf(appQuota, func(q model.Quota) *int64 { return q.MonthlyBudget() }),
		ptrOf(orgQuota, func(q model.Quota) *int64 { return q.MonthlyBudget() }),
	)
	effective.YearlyBudget = minPtrSvc(
		ptrOf(appQuota, func(q model.Quota) *int64 { return q.YearlyBudget() }),
		ptrOf(orgQuota, func(q model.Quota) *int64 { return q.YearlyBudget() }),
	)

	return effective, nil
}

func ptrOf(q model.Quota, f func(model.Quota) *int64) *int64 {
	if q == nil {
		return nil
	}
	return f(q)
}

func minPtrSvc(a, b *int64) *int64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a < *b {
		return a
	}
	return b
}
