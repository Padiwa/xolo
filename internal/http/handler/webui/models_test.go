package webui

import (
	"testing"
	"time"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/http/handler/webui/profile/component"
)

// stubLLMModel lets tests build an LLMModel with only the cost/virtual bits
// the comparator looks at, without standing up the full model.
type stubLLMModel struct {
	id         model.LLMModelID
	prompt     int64
	completion int64
	virtual    bool
}

func (s *stubLLMModel) ID() model.LLMModelID                      { return s.id }
func (s *stubLLMModel) PromptCostPer1KTokens() int64              { return s.prompt }
func (s *stubLLMModel) CachedPromptCostPer1KTokens() int64        { return s.prompt }
func (s *stubLLMModel) CompletionCostPer1KTokens() int64          { return s.completion }
func (s *stubLLMModel) IsVirtual() bool                           { return s.virtual }
func (s *stubLLMModel) ProviderID() model.ProviderID              { return "" }
func (s *stubLLMModel) OrgID() model.OrgID                        { return "" }
func (s *stubLLMModel) ProxyName() string                         { return "" }
func (s *stubLLMModel) RealModel() string                         { return "" }
func (s *stubLLMModel) Description() string                       { return "" }
func (s *stubLLMModel) Enabled() bool                             { return true }
func (s *stubLLMModel) ContextWindow() int64                      { return 0 }
func (s *stubLLMModel) OutputWindow() int64                       { return 0 }
func (s *stubLLMModel) ActiveParams() int64                       { return 0 }
func (s *stubLLMModel) TokensPerSecLow() float64                  { return 0 }
func (s *stubLLMModel) TokensPerSecHigh() float64                 { return 0 }
func (s *stubLLMModel) Capabilities() model.ModelCapabilities     { return model.ModelCapabilities{} }
func (s *stubLLMModel) CreatedAt() time.Time                      { return time.Time{} }
func (s *stubLLMModel) UpdatedAt() time.Time                      { return time.Time{} }
func (s *stubLLMModel) TokenLimitConfig() *model.TokenLimitConfig { return nil }
func (s *stubLLMModel) ExtraBody() map[string]any                 { return nil }

var _ model.LLMModel = (*stubLLMModel)(nil)

func usage(requests int64) *port.UsageAggregate {
	return &port.UsageAggregate{TotalRequests: requests}
}

func realMu(id string, prompt, completion int64, agg *port.UsageAggregate) component.ModelUsage {
	return component.ModelUsage{
		Model:     &stubLLMModel{id: model.LLMModelID(id), prompt: prompt, completion: completion},
		Aggregate: agg,
	}
}

func virtualMu(id string) component.ModelUsage {
	return component.ModelUsage{
		Model: &stubLLMModel{id: model.LLMModelID(id), virtual: true},
	}
}

func TestCompareModelUsages(t *testing.T) {
	t.Run("price sort pushes virtual models to the end", func(t *testing.T) {
		v := virtualMu("v")
		cheap := realMu("cheap", 1, 1, usage(0))

		if compareModelUsages(v, cheap, "price") {
			t.Errorf("virtual model must NOT outrank a real model when sorting by price")
		}
		if !compareModelUsages(cheap, v, "price") {
			t.Errorf("real model with positive cost MUST outrank a virtual model when sorting by price")
		}
	})

	t.Run("price sort is ascending on prompt + completion", func(t *testing.T) {
		cheap := realMu("cheap", 1, 1, usage(0))
		expensive := realMu("expensive", 1000, 1000, usage(0))

		if !compareModelUsages(cheap, expensive, "price") {
			t.Errorf("cheaper model should outrank more expensive when sorting by price")
		}
		if compareModelUsages(expensive, cheap, "price") {
			t.Errorf("more expensive model must NOT outrank cheaper when sorting by price")
		}
	})

	t.Run("two virtual models fall back to usage descending under price sort", func(t *testing.T) {
		hi := virtualMu("v-high")
		hi.Aggregate = usage(10)
		lo := virtualMu("v-low")
		lo.Aggregate = usage(1)

		if !compareModelUsages(hi, lo, "price") {
			t.Errorf("two virtual models under price sort should fall back to usage descending (more requests first)")
		}
		if compareModelUsages(lo, hi, "price") {
			t.Errorf("two virtual models under price sort should fall back to usage descending (fewer requests last)")
		}
	})

	t.Run("two virtual models with no usage are considered equal", func(t *testing.T) {
		a := virtualMu("v1")
		b := virtualMu("v2")

		if compareModelUsages(a, b, "price") {
			t.Errorf("two virtual models with no usage should be considered equal")
		}
		if compareModelUsages(b, a, "price") {
			t.Errorf("two virtual models with no usage should be considered equal (other direction)")
		}
	})

	t.Run("tied cost falls back to usage descending", func(t *testing.T) {
		high := realMu("tied-high", 5, 5, usage(10))
		low := realMu("tied-low", 5, 5, usage(1))

		if !compareModelUsages(high, low, "price") {
			t.Errorf("tied cost should fall back to usage comparison (more requests first)")
		}
		if compareModelUsages(low, high, "price") {
			t.Errorf("tied cost should fall back to usage comparison (fewer requests last)")
		}
	})

	t.Run("tied cost with nil aggregates is equal", func(t *testing.T) {
		a := realMu("tied-nil-1", 7, 7, nil)
		b := realMu("tied-nil-2", 7, 7, nil)

		if compareModelUsages(a, b, "price") {
			t.Errorf("tied cost with nil aggregates should be considered equal")
		}
		if compareModelUsages(b, a, "price") {
			t.Errorf("tied cost with nil aggregates should be considered equal (other direction)")
		}
	})

	t.Run("nil aggregate sorts after non-nil aggregate under price tie", func(t *testing.T) {
		withAgg := realMu("with-agg", 7, 7, usage(0))
		noAgg := realMu("no-agg", 7, 7, nil)

		if !compareModelUsages(withAgg, noAgg, "price") {
			t.Errorf("non-nil aggregate should outrank nil aggregate on tied cost")
		}
		if compareModelUsages(noAgg, withAgg, "price") {
			t.Errorf("nil aggregate must NOT outrank non-nil aggregate on tied cost")
		}
	})

	t.Run("default sort falls back to usage descending", func(t *testing.T) {
		hi := realMu("hi", 0, 0, usage(5))
		lo := realMu("lo", 0, 0, usage(1))

		if !compareModelUsages(hi, lo, "") {
			t.Errorf("default sort should order by usage descending")
		}
		if compareModelUsages(lo, hi, "") {
			t.Errorf("default sort must NOT order by usage ascending")
		}
	})

	t.Run("unknown sort value falls back to default ordering", func(t *testing.T) {
		hi := realMu("hi", 0, 0, usage(5))
		lo := realMu("lo", 0, 0, usage(1))

		if !compareModelUsages(hi, lo, "price_invalid") {
			t.Errorf("unknown sort values should fall back to default usage ordering")
		}
	})

	t.Run("default sort is independent of cost", func(t *testing.T) {
		// A cheap model with no usage must NOT outrank an expensive model
		// with usage under the default sort.
		cheapUnused := realMu("cheap-unused", 1, 1, usage(0))
		expensiveUsed := realMu("expensive-used", 1000, 1000, usage(5))

		if compareModelUsages(cheapUnused, expensiveUsed, "") {
			t.Errorf("default sort should NOT be affected by cost")
		}
		if !compareModelUsages(expensiveUsed, cheapUnused, "") {
			t.Errorf("default sort should keep higher usage first regardless of cost")
		}
	})
}
