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
}

// An application token carries an ApplicationID in metadata. A request whose
// application counter exceeds the application budget must be rejected even
// though the shadow user has no budget of its own: this is the gap issue #64
// describes. Without the application check the request would silently slip
// through to the org budget.
func TestQuotaEnforcerEnforcesApplicationQuota(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")

	// The user-scope check passes: the shadow user has no budget of its own
	// and the application counter is what we expect to trip.
	now := time.Now()
	usage := &fakeUsage{spent: map[time.Time]int64{
		model.StartOfDay(now): 0, // user scope: nothing spent (the shadow user is not a counter)
	}}
	resolver := appQuotaResolver{
		userQuota: &model.EffectiveQuota{Currency: "EUR"}, // no user budget
		appQuota:  &model.EffectiveQuota{DailyBudget: i64(60_000), Currency: "EUR"},
	}
	enforcer := NewXoloQuotaEnforcer(resolver, &noOrgQuotaStore{}, usage, &fakeProviderStore{provider: provider, llmModel: llmModel})

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
