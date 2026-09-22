package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	genaiProxy "github.com/bornholm/genai/proxy"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/pipeline"
)

// unknownModelStore answers every lookup with ErrNotFound, as the store does
// for a virtual model name: it has no row in the LLM model table.
type unknownModelStore struct {
	port.ProviderStore
}

func (s *unknownModelStore) GetLLMModelByID(context.Context, model.LLMModelID) (model.LLMModel, error) {
	return nil, port.ErrNotFound
}

func (s *unknownModelStore) GetLLMModelByProxyName(context.Context, model.OrgID, string) (model.LLMModel, error) {
	return nil, port.ErrNotFound
}

// exhaustedEnforcer builds an enforcer whose user has spent its whole daily budget.
func exhaustedEnforcer(providerStore port.ProviderStore) *XoloQuotaEnforcer {
	usage := &fakeUsage{spent: map[time.Time]int64{
		model.StartOfDay(time.Now()): 1_000_000,
	}}
	resolver := fakeQuotaResolver{quota: &model.EffectiveQuota{
		DailyBudget: i64(1_000),
		Currency:    "EUR",
	}}
	return NewXoloQuotaEnforcer(resolver, nil, usage, providerStore)
}

func quotaTestRequest(exec *pipeline.ForwardExecution) *genaiProxy.ProxyRequest {
	req := &genaiProxy.ProxyRequest{
		UserID:   "user-1",
		Model:    "acme/support-agent",
		Metadata: map[string]any{MetaOrgID: "org-1"},
	}
	if exec != nil {
		req.Metadata[metaPipelineExecution] = exec
	}
	return req
}

// A pipeline whose terminal node forges its own response never reaches a
// provider: an exhausted budget must not turn that answer into a 429.
func TestQuotaEnforcerSkipsPipelineAnswerWithoutProvider(t *testing.T) {
	enforcer := exhaustedEnforcer(&unknownModelStore{})
	exec := &pipeline.ForwardExecution{
		ResolvedClient: NewDummyLLMClient("Je ne réponds qu'aux questions sur le service.", "dummy"),
		ResolvedModel:  "dummy",
	}

	result, err := enforcer.PreRequest(context.Background(), quotaTestRequest(exec))
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result != nil {
		t.Fatalf("expected the request to pass, got response %+v", result.Response)
	}
}

// A pipeline that resolved a real model stays under quota enforcement.
func TestQuotaEnforcerEnforcesPipelineAnswerFromProvider(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	enforcer := exhaustedEnforcer(&fakeProviderStore{provider: provider, llmModel: llmModel})
	req := quotaTestRequest(&pipeline.ForwardExecution{
		ResolvedClient:  NewDummyLLMClient("hello", "qwen"),
		ResolvedModel:   "qwen",
		ResolvedModelID: llmModel.ID(),
	})
	req.Metadata[MetaModelID] = string(llmModel.ID())

	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result == nil || result.Response == nil || result.Response.StatusCode != 429 {
		t.Fatalf("expected a 429 quota response, got %+v", result)
	}
	// Pin the user-scope message subject so the new "User" prefix cannot
	// silently regress to "Daily budget exceeded" without the test suite noticing.
	body, _ := result.Response.Body.(map[string]any)
	errBody, _ := body["error"].(map[string]any)
	msg, _ := errBody["message"].(string)
	if !strings.Contains(msg, "User daily budget exceeded") {
		t.Errorf("expected user daily budget error, got %q", msg)
	}
}

// An application token carries an ApplicationID in metadata. A request whose
// application counter exceeds the application budget must be rejected even
// though the shadow user has no budget of its own: this is the gap issue #64
// describes. Without the application check the request would silently slip
// through to the org budget.
//
// The per-user check is intentionally skipped on this path: the shadow user's
// counter is never fed for an application record, and resolving it would cost
// an extra GetOrgByID + ListOrgMembers when ShareQuotaEqually is on. The
// countingResolver below asserts that ResolveEffectiveQuota is not called at
// all for an application request.
func TestQuotaEnforcerEnforcesApplicationQuota(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	// The application counter is what we expect to trip. The user-scope
	// resolver is wired to the counting wrapper so we can assert it is
	// never called for an application request — the shadow user is a
	// routing artifact that has no budget of its own.
	now := time.Now()
	usage := &fakeUsage{spent: map[time.Time]int64{
		model.StartOfDay(now): 0, // user scope: nothing spent (the shadow user is not a counter)
	}}
	counting := &countingQuotaResolver{
		appQuotaResolver: appQuotaResolver{
			userQuota: &model.EffectiveQuota{Currency: "EUR"}, // no user budget
			appQuota:  &model.EffectiveQuota{DailyBudget: i64(60_000), Currency: "EUR"},
		},
	}
	enforcer := NewXoloQuotaEnforcer(counting, &noOrgQuotaStore{}, usage, &fakeProviderStore{provider: provider, llmModel: llmModel})

	req := quotaTestRequest(&pipeline.ForwardExecution{
		ResolvedClient:  NewDummyLLMClient("hello", "qwen"),
		ResolvedModel:   "qwen",
		ResolvedModelID: llmModel.ID(),
	})
	req.Metadata[MetaModelID] = string(llmModel.ID())
	req.Metadata[MetaApplicationID] = "app-acme-ci"

	// First call: nothing spent yet, the application check passes.
	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result != nil {
		t.Fatalf("expected the request to pass, got response %+v", result.Response)
	}
	if counting.userCalls != 0 {
		t.Errorf("ResolveEffectiveQuota was called %d time(s) for an application request, want 0", counting.userCalls)
	}
	if counting.appCalls != 1 {
		t.Errorf("ResolveEffectiveQuotaForApplication was called %d time(s), want 1", counting.appCalls)
	}

	// Application counter has now reached the budget: the next request is rejected.
	usage.spent[model.StartOfDay(now)] = 60_000
	result, err = enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result == nil || result.Response == nil || result.Response.StatusCode != 429 {
		t.Fatalf("expected a 429 application quota response, got %+v", result)
	}
	body, _ := result.Response.Body.(map[string]any)
	errBody, _ := body["error"].(map[string]any)
	if msg, _ := errBody["message"].(string); msg == "" || !strings.Contains(msg, "Application daily budget exceeded") {
		t.Errorf("expected application daily budget error, got %q", msg)
	}
}

// countingQuotaResolver wraps appQuotaResolver to count how many times each
// method is called. The application path must invoke the application resolver
// exactly once and the user resolver never.
type countingQuotaResolver struct {
	appQuotaResolver
	userCalls int
	appCalls  int
}

func (r *countingQuotaResolver) ResolveEffectiveQuota(ctx context.Context, u model.UserID, o model.OrgID) (*model.EffectiveQuota, error) {
	r.userCalls++
	return r.appQuotaResolver.ResolveEffectiveQuota(ctx, u, o)
}

func (r *countingQuotaResolver) ResolveEffectiveQuotaForApplication(ctx context.Context, a model.ApplicationID, o model.OrgID) (*model.EffectiveQuota, error) {
	r.appCalls++
	return r.appQuotaResolver.ResolveEffectiveQuotaForApplication(ctx, a, o)
}

// An application budget that is set only on the monthly or yearly periods
// must still trigger the right branch — the daily coverage above does not
// exercise the StartOfMonth / StartOfYear lookups, the error messages or the
// currency plumbing.
func TestQuotaEnforcerEnforcesApplicationQuotaMonthlyAndYearly(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	now := time.Now()
	baseReq := func() *genaiProxy.ProxyRequest {
		req := quotaTestRequest(&pipeline.ForwardExecution{
			ResolvedClient:  NewDummyLLMClient("hello", "qwen"),
			ResolvedModel:   "qwen",
			ResolvedModelID: llmModel.ID(),
		})
		req.Metadata[MetaModelID] = string(llmModel.ID())
		req.Metadata[MetaApplicationID] = "app-acme-ci"
		return req
	}

	tests := []struct {
		name           string
		spent          map[time.Time]int64
		appQuota       *model.EffectiveQuota
		wantStatusCode int
		wantMessage    string
	}{
		{
			name: "monthly budget exhausted",
			spent: map[time.Time]int64{
				model.StartOfMonth(now): 1_500_000,
			},
			appQuota:       &model.EffectiveQuota{MonthlyBudget: i64(1_500_000), Currency: "EUR"},
			wantStatusCode: 429,
			wantMessage:    "Application monthly budget exceeded",
		},
		{
			name: "yearly budget exhausted",
			spent: map[time.Time]int64{
				model.StartOfYear(now): 20_000_000,
			},
			appQuota:       &model.EffectiveQuota{YearlyBudget: i64(20_000_000), Currency: "EUR"},
			wantStatusCode: 429,
			wantMessage:    "Application yearly budget exceeded",
		},
		{
			name:  "monthly and yearly below budget: passes",
			spent: map[time.Time]int64{model.StartOfMonth(now): 0, model.StartOfYear(now): 0},
			appQuota: &model.EffectiveQuota{
				MonthlyBudget: i64(1_500_000),
				YearlyBudget:  i64(20_000_000),
				Currency:      "EUR",
			},
			wantStatusCode: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enforcer := NewXoloQuotaEnforcer(
				appQuotaResolver{
					userQuota: &model.EffectiveQuota{Currency: "EUR"},
					appQuota:  tc.appQuota,
				},
				&noOrgQuotaStore{},
				&fakeUsage{spent: tc.spent},
				&fakeProviderStore{provider: provider, llmModel: llmModel},
			)

			result, err := enforcer.PreRequest(context.Background(), baseReq())
			if err != nil {
				t.Fatalf("PreRequest() error = %v", err)
			}
			if tc.wantStatusCode == 0 {
				if result != nil {
					t.Fatalf("expected the request to pass, got %+v", result.Response)
				}
				return
			}
			if result == nil || result.Response == nil || result.Response.StatusCode != tc.wantStatusCode {
				t.Fatalf("expected status %d, got %+v", tc.wantStatusCode, result)
			}
			body, _ := result.Response.Body.(map[string]any)
			errBody, _ := body["error"].(map[string]any)
			msg, _ := errBody["message"].(string)
			if !strings.Contains(msg, tc.wantMessage) {
				t.Errorf("expected error %q, got %q", tc.wantMessage, msg)
			}
		})
	}
}

// Without an ApplicationID in metadata the application path must not run at
// all, even when the resolver would return a non-nil application quota. The
// shadow user's user-scope check still applies as before.
func TestQuotaEnforcerSkipsApplicationPathWithoutAppID(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	enforcer := NewXoloQuotaEnforcer(
		appQuotaResolver{
			userQuota: &model.EffectiveQuota{DailyBudget: i64(1_000), Currency: "EUR"},
		},
		&noOrgQuotaStore{},
		&fakeUsage{spent: map[time.Time]int64{model.StartOfDay(time.Now()): 0}},
		&fakeProviderStore{provider: provider, llmModel: llmModel},
	)

	req := quotaTestRequest(&pipeline.ForwardExecution{
		ResolvedClient:  NewDummyLLMClient("hello", "qwen"),
		ResolvedModel:   "qwen",
		ResolvedModelID: llmModel.ID(),
	})
	req.Metadata[MetaModelID] = string(llmModel.ID())
	// Deliberately no MetaApplicationID.

	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result != nil {
		t.Fatalf("expected the request to pass without an app id, got %+v", result.Response)
	}
}

// Application budget under the cap, organisation budget exhausted: the
// org-wide branch must still trip on the application path. The application
// resolver is called first and succeeds; the org-wide branch then reads the
// org quota and rejects. This is the mirror of the existing
// TestQuotaEnforcerEnforcesApplicationQuota, which uses noOrgQuotaStore to
// avoid this branch entirely — the gap a future guard on appID=="" around
// the org-wide check would silently reopen.
func TestQuotaEnforcerEnforcesOrgBudgetWhenAppUnderLimit(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	now := time.Now()
	counting := &countingQuotaResolver{
		appQuotaResolver: appQuotaResolver{
			// Application budget is generous; the request stays under it.
			appQuota: &model.EffectiveQuota{DailyBudget: i64(1_000_000), Currency: "EUR"},
		},
	}
	// Org quota: daily budget at zero, so any non-negative spend exceeds it.
	enforcer := NewXoloQuotaEnforcer(
		counting,
		&exhaustedOrgQuotaStore{daily: 0},
		&fakeUsage{spent: map[time.Time]int64{model.StartOfDay(now): 1_000}},
		&fakeProviderStore{provider: provider, llmModel: llmModel},
	)

	req := quotaTestRequest(&pipeline.ForwardExecution{
		ResolvedClient:  NewDummyLLMClient("hello", "qwen"),
		ResolvedModel:   "qwen",
		ResolvedModelID: llmModel.ID(),
	})
	req.Metadata[MetaModelID] = string(llmModel.ID())
	req.Metadata[MetaApplicationID] = "app-acme-ci"

	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result == nil || result.Response == nil || result.Response.StatusCode != 429 {
		t.Fatalf("expected a 429 organisation quota response, got %+v", result)
	}
	body, _ := result.Response.Body.(map[string]any)
	errBody, _ := body["error"].(map[string]any)
	if msg, _ := errBody["message"].(string); msg == "" || !strings.Contains(msg, "Organization daily budget exceeded") {
		t.Errorf("expected organisation daily budget error, got %q", msg)
	}
	// The application resolver was consulted before the org branch tripped.
	if counting.appCalls != 1 {
		t.Errorf("ResolveEffectiveQuotaForApplication was called %d time(s), want 1", counting.appCalls)
	}
}

// Both the application and the organisation budgets are exhausted at once.
// The application-scope check runs first inside PreRequest and returns its
// 429; the organisation branch never executes. Pin the ordering so a future
// refactor cannot swap the two error messages without the test suite noticing.
func TestQuotaEnforcerPrefersApplicationErrorWhenBothExhausted(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	now := time.Now()
	counting := &countingQuotaResolver{
		appQuotaResolver: appQuotaResolver{
			// Application budget: 60 000 µ¢/day, primed to the cap.
			appQuota: &model.EffectiveQuota{DailyBudget: i64(60_000), Currency: "EUR"},
		},
	}
	usage := &fakeUsage{spent: map[time.Time]int64{
		// Application counter exactly at the cap; the org counter would also
		// be over its own cap if the enforcer ever reached that branch.
		model.StartOfDay(now): 60_000,
	}}
	enforcer := NewXoloQuotaEnforcer(
		counting,
		&exhaustedOrgQuotaStore{daily: 0},
		usage,
		&fakeProviderStore{provider: provider, llmModel: llmModel},
	)

	req := quotaTestRequest(&pipeline.ForwardExecution{
		ResolvedClient:  NewDummyLLMClient("hello", "qwen"),
		ResolvedModel:   "qwen",
		ResolvedModelID: llmModel.ID(),
	})
	req.Metadata[MetaModelID] = string(llmModel.ID())
	req.Metadata[MetaApplicationID] = "app-acme-ci"

	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result == nil || result.Response == nil || result.Response.StatusCode != 429 {
		t.Fatalf("expected a 429, got %+v", result)
	}
	body, _ := result.Response.Body.(map[string]any)
	errBody, _ := body["error"].(map[string]any)
	msg, _ := errBody["message"].(string)
	if !strings.Contains(msg, "Application daily budget exceeded") {
		t.Errorf("expected application daily budget error, got %q", msg)
	}
	if strings.Contains(msg, "Organization") {
		t.Errorf("organisation message leaked even though the application check ran first: %q", msg)
	}
	// The application branch tripped, so the org branch should not have run.
	if counting.appCalls != 1 {
		t.Errorf("ResolveEffectiveQuotaForApplication was called %d time(s), want 1", counting.appCalls)
	}
}

// appQuotaResolver separates the answers for user and application paths so the
// tests can exercise each independently. The two effective quotas are merged
// at call time; the per-principal fields exist purely so each test case can
// spell out what it expects.
type appQuotaResolver struct {
	userQuota *model.EffectiveQuota
	appQuota  *model.EffectiveQuota
	err       error
}

func (r appQuotaResolver) ResolveEffectiveQuota(context.Context, model.UserID, model.OrgID) (*model.EffectiveQuota, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.userQuota, nil
}

func (r appQuotaResolver) ResolveEffectiveQuotaForApplication(context.Context, model.ApplicationID, model.OrgID) (*model.EffectiveQuota, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.appQuota, nil
}

// noOrgQuotaStore answers every quota lookup with ErrNotFound: the enforcer
// should treat this as "no quota set" and skip the org-wide check, the same
// way a store with no quota row does.
type noOrgQuotaStore struct {
	port.QuotaStore
}

func (s *noOrgQuotaStore) GetQuota(context.Context, model.QuotaScope, string) (model.Quota, error) {
	return nil, port.ErrNotFound
}

func (s *noOrgQuotaStore) ResolveEffectiveQuota(context.Context, model.UserID, model.OrgID) (*model.EffectiveQuota, error) {
	return &model.EffectiveQuota{}, nil
}

func (s *noOrgQuotaStore) ResolveEffectiveQuotaForApplication(context.Context, model.ApplicationID, model.OrgID) (*model.EffectiveQuota, error) {
	return &model.EffectiveQuota{}, nil
}

// exhaustedOrgQuotaStore returns a fixed org quota with a daily budget at zero:
// the enforcer's org-wide check is expected to trip on this. The mirror case
// (application budget not exhausted but org budget exceeded) is what the
// TestQuotaEnforcerEnforcesOrgBudgetWhenAppUnderLimit test pins: a regression
// that guarded the org-wide branch on appID=="" would silently bypass the
// org limit for application requests.
type exhaustedOrgQuotaStore struct {
	port.QuotaStore
	daily int64
}

func (s *exhaustedOrgQuotaStore) GetQuota(_ context.Context, scope model.QuotaScope, _ string) (model.Quota, error) {
	if scope != model.QuotaScopeOrg {
		return nil, port.ErrNotFound
	}
	return &fakeQuota{scope: scope, currency: "EUR", daily: &s.daily}, nil
}

func (s *exhaustedOrgQuotaStore) ResolveEffectiveQuota(context.Context, model.UserID, model.OrgID) (*model.EffectiveQuota, error) {
	return &model.EffectiveQuota{}, nil
}

func (s *exhaustedOrgQuotaStore) ResolveEffectiveQuotaForApplication(context.Context, model.ApplicationID, model.OrgID) (*model.EffectiveQuota, error) {
	return &model.EffectiveQuota{}, nil
}

// fakeQuota implements model.Quota with the minimum the enforcer reads.
type fakeQuota struct {
	scope    model.QuotaScope
	currency string
	daily    *int64
	monthly  *int64
	yearly   *int64
}

func (q *fakeQuota) ID() model.QuotaID       { return "q" }
func (q *fakeQuota) Scope() model.QuotaScope { return q.scope }
func (q *fakeQuota) ScopeID() string         { return "" }
func (q *fakeQuota) Currency() string        { return q.currency }
func (q *fakeQuota) DailyBudget() *int64     { return q.daily }
func (q *fakeQuota) MonthlyBudget() *int64   { return q.monthly }
func (q *fakeQuota) YearlyBudget() *int64    { return q.yearly }
func (q *fakeQuota) CreatedAt() time.Time    { return time.Time{} }
func (q *fakeQuota) UpdatedAt() time.Time    { return time.Time{} }

var _ model.Quota = (*fakeQuota)(nil)
