package proxy

import (
	"context"
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

// Issue #82: the enforcer must use the org quota carried on EffectiveQuota
// instead of issuing a second quotaStore.GetQuota(QuotaScopeOrg, ...) on the
// hot path. We assert that by setting OrgQuota to a record that *would*
// reject the request if read, and by wiring a resolver that does not load
// anything — if the enforcer still re-reads, the request would slip through.
func TestQuotaEnforcerReusesOrgQuotaFromResolver(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")
	providerStore := &fakeProviderStore{provider: provider, llmModel: llmModel}

	// The resolver returns a merged user budget above the spent amount (so
	// the per-user check passes) and an OrgQuota whose daily budget is below
	// the spent amount (so the org-wide block must trip). The org-wide block
	// is fed by EffectiveQuota.OrgQuota, not by a fresh GetQuota call.
	spent := int64(2_000)
	usage := &fakeUsage{spent: map[time.Time]int64{
		model.StartOfDay(time.Now()): spent,
	}}
	orgQuota := model.NewQuota(model.QuotaScopeOrg, string(orgID), "EUR",
		i64(1_000), // tighter than the per-user daily budget — will trip
		nil, nil,
	)
	resolver := fakeQuotaResolver{quota: &model.EffectiveQuota{
		DailyBudget: i64(10_000), // well above the spent amount
		Currency:    "EUR",
		OrgQuota:    orgQuota,
	}}
	enforcer := NewXoloQuotaEnforcer(resolver, nil, usage, providerStore)

	req := &genaiProxy.ProxyRequest{
		UserID:   "user-1",
		Model:    "acme/qwen-strong",
		Metadata: map[string]any{MetaOrgID: string(orgID), MetaModelID: string(llmModel.ID())},
	}

	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result == nil || result.Response == nil || result.Response.StatusCode != 429 {
		t.Fatalf("expected a 429 quota response from the org-wide block, got %+v", result)
	}
}

// Issue #82: when the resolver reports no org quota (OrgQuota == nil), the
// org-wide block must be skipped — equivalent to a GetQuota that returned
// port.ErrNotFound. The enforcer must not crash on the nil OrgQuota either.
func TestQuotaEnforcerSkipsOrgBlockWhenOrgQuotaNil(t *testing.T) {
	orgID := model.OrgID("org-1")
	providerID := model.NewProviderID()
	llmModel := model.NewLLMModel(providerID, orgID, "acme/qwen-strong", "qwen", "desc", 1000, 2000)
	provider := model.NewProvider(orgID, "ollama", "openai", "http://localhost:11434/v1", "key", "EUR")
	providerStore := &fakeProviderStore{provider: provider, llmModel: llmModel}

	usage := &fakeUsage{spent: map[time.Time]int64{}}
	resolver := fakeQuotaResolver{quota: &model.EffectiveQuota{
		DailyBudget: nil, // no per-user daily limit either
		Currency:    "EUR",
		OrgQuota:    nil, // no org quota on file
	}}
	enforcer := NewXoloQuotaEnforcer(resolver, nil, usage, providerStore)

	req := &genaiProxy.ProxyRequest{
		UserID:   "user-1",
		Model:    "acme/qwen-strong",
		Metadata: map[string]any{MetaOrgID: string(orgID), MetaModelID: string(llmModel.ID())},
	}

	result, err := enforcer.PreRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("PreRequest() error = %v", err)
	}
	if result != nil {
		t.Fatalf("expected the request to pass when OrgQuota is nil, got %+v", result)
	}
}
