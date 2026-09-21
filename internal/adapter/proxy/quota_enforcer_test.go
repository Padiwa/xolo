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
