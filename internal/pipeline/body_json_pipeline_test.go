package pipeline_test

import (
	"context"
	"testing"

	"github.com/xolo-gateway/xolo/internal/pipeline/pipelinetest"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

// TestPipeline_BodyJSONFlowsToPreRequestInputModel is the end-to-end regression
// test for issue #34. It exercises the full chain that the original bug broke:
//
//	engine.go:77  seeds the generator node's "request" port with ec.BodyJSON
//	plugin_executor.go:156  reads it back as PreRequestInput.Model
//
// before issue #34, every consumer along this chain saw "" at runtime because
// ExecutionContext.RequestJSON (the field that engine.go:77 used to read) was
// declared but never assigned by buildEC / buildMiddlewareEC. The fix
// collapses RequestJSON into BodyJSON so a single source of truth — the body
// of the proxied LLM request — flows all the way through.
//
// This test runs a Generator -> Plugin(PRE_REQUEST) -> Model -> Sink graph
// through Harness.Run with WithBodyJSON(...) seeding the EC, and asserts that
// the captured value of PreRequestInput.Model equals the seeded body. A
// future regression that re-broke either engine.go:77 (seed) or
// plugin_executor.go:156 (consume) would flip this test red.
func TestPipeline_BodyJSONFlowsToPreRequestInputModel(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}],"temperature":0.7}`

	var capturedModel string
	capturePlugin := &pipelinetest.PluginClient{
		PreRequestFunc: func(_ context.Context, in *proto.PreRequestInput) (*proto.PreRequestOutput, error) {
			capturedModel = in.GetModel()
			return &proto.PreRequestOutput{Allowed: true}, nil
		},
	}

	plugins := pipelinetest.NewPluginProvider().
		Register("capture", pipelinetest.PreRequestDescriptor("capture"), capturePlugin)

	resolver := pipelinetest.NewModelResolver().WithResponse("org/gpt4", "ok")

	graph := pipelinetest.NewGraph().
		Generator("gen").
		Plugin("capture", "capture").
		ModelWithProxy("mdl", "org/gpt4").
		Sink("sink").
		Edge("gen", "request", "capture", "request").
		Edge("capture", "request", "mdl", "request").
		Edge("mdl", "response", "sink", "response").
		Build()

	h := pipelinetest.New(
		pipelinetest.WithPlugins(plugins),
		pipelinetest.WithModelResolver(resolver),
	)

	ec := pipelinetest.NewExecutionContext(pipelinetest.WithBodyJSON(body))

	result, err := h.Run(context.Background(), graph, ec)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.Rejected {
		t.Fatalf("unexpected rejection: %s", result.RejectionReason)
	}

	if capturedModel != body {
		t.Fatalf("PreRequestInput.Model = %q, want %q (full LLM request body seeded by buildEC)", capturedModel, body)
	}
	if capturedModel == "" {
		t.Fatal("PreRequestInput.Model must be non-empty; engine.go:77 must seed it from ec.BodyJSON and plugin_executor.go:156 must forward it")
	}
}
