package pipeline_test

import (
	"context"
	"testing"

	"github.com/xolo-gateway/xolo/internal/pipeline/pipelinetest"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

// TestPipeline_BodyJSONFlowsToPreRequestInputModel is the end-to-end regression
// test for issue #34. It runs a Generator -> Plugin(PRE_REQUEST) -> Model ->
// Sink graph through Harness.Run with WithBodyJSON(...) seeding the EC, and
// asserts that PreRequestInput.Model equals the seeded body.
//
// What this test locks down:
//   - plugin_executor.go:156 reads ec.BodyJSON into PreRequestInput.Model.
//     A regression that reverted to ec.RequestJSON (no longer compiles) or to
//     "" would flip this test red.
//
// What this test does *not* lock down on its own:
//   - engine.go:77's seed step, which writes ec.BodyJSON into the
//     ValueContext under (generator-id, "request"), is *redundantly* covered:
//     GeneratorExecutor.Forward also writes ec.BodyJSON to its "request"
//     output (engine.go:113-115), and the plugin's ResolveInputs reads from
//     the ValueContext after that overwrite. A regression in engine.go:77
//     alone is masked. TestGeneratorExecutor_RequestPortCarriesBodyJSON in
//     internal/adapter/proxy covers the generator's re-emit; together, the
//     two tests cover both writer sites.
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
		t.Fatalf("PreRequestInput.Model = %q, want %q (full LLM request body sourced from ec.BodyJSON at plugin_executor.go:156)", capturedModel, body)
	}
}
