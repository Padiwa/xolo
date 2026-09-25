package pipeline_test

import (
	"context"
	"testing"

	"github.com/bornholm/genai/llm"
	"github.com/xolo-gateway/xolo/internal/pipeline/pipelinetest"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

// TestPipeline_ToolProviderPlugin wires Generator -> Plugin(TOOL_PROVIDER) ->
// Model -> Sink and verifies that the resolved client (a) calls the
// plugin's CallTool RPC to resolve a matching tool_call, and (b) loops back
// to the underlying model client until a final answer is produced.
func TestPipeline_ToolProviderPlugin(t *testing.T) {
	var callToolInvocations []*proto.CallToolInput

	pluginClient := &pipelinetest.PluginClient{
		ListToolsFunc: func(_ context.Context, _ *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
			return &proto.ListToolsOutput{
				Tools: []*proto.ToolDescriptor{
					{Name: "search", Description: "search the web"},
				},
			}, nil
		},
		CallToolFunc: func(_ context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
			callToolInvocations = append(callToolInvocations, in)
			return &proto.CallToolOutput{ResultText: "42"}, nil
		},
	}

	plugins := pipelinetest.NewPluginProvider().
		Register("tool-bridge", pipelinetest.ToolProviderDescriptor("tool-bridge"), pluginClient)

	toolCall := llm.NewToolCall("call-1", "search", `{"q":"life, the universe and everything"}`)
	modelClient := &scriptedClient{responses: []*scriptedResponse{
		{toolCalls: []llm.ToolCall{toolCall}},
		{content: "the answer is 42"},
	}}
	resolver := pipelinetest.NewModelResolver().WithModel("org/gpt", modelClient)

	graph := pipelinetest.NewGraph().
		Generator("gen").
		Plugin("tools", "tool-bridge").
		ModelWithProxy("mdl", "org/gpt").
		Sink("sink").
		Edge("gen", "request", "tools", "request").
		Edge("tools", "request", "mdl", "request").
		Edge("mdl", "response", "sink", "response").
		Build()

	h := pipelinetest.New(
		pipelinetest.WithPlugins(plugins),
		pipelinetest.WithModelResolver(resolver),
	)

	exec, err := h.Engine().RunForward(context.Background(), graph, pipelinetest.NewExecutionContext())
	if err != nil {
		t.Fatalf("RunForward failed: %v", err)
	}
	if exec.ResolvedClient == nil {
		t.Fatal("expected a resolved client, got nil")
	}

	resp, err := exec.ResolvedClient.ChatCompletion(context.Background(), llm.WithMessages(llm.NewMessage(llm.RoleUser, "what is the answer?")))
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.Message().Content() != "the answer is 42" {
		t.Errorf("expected final answer, got %q", resp.Message().Content())
	}
	if len(callToolInvocations) != 1 {
		t.Fatalf("expected exactly 1 CallTool invocation, got %d", len(callToolInvocations))
	}
	if callToolInvocations[0].Name != "search" {
		t.Errorf("expected CallTool for %q, got %q", "search", callToolInvocations[0].Name)
	}
	if len(modelClient.calls) != 2 {
		t.Errorf("expected 2 model calls (tool round + final), got %d", len(modelClient.calls))
	}
}

// TestPipeline_PostResponsePluginReceivesResolvedModel wires
// Generator -> Plugin(POST_RESPONSE) -> Model -> Sink and runs the full
// forward + backward cycle. The plugin's PostResponse RPC must see the
// identifier of the model that actually answered, not an empty string.
//
// Regression for https://github.com/xolo-gateway/xolo/issues/83: the plugin
// contract documents PostResponseInput.Model as "Model that was called",
// but PluginExecutor.Backward used to hardcode an empty string. A test that
// only hand-builds BackwardInput would have missed the bug, so this one
// drives the real Engine.RunBackward path.
func TestPipeline_PostResponsePluginReceivesResolvedModel(t *testing.T) {
	var postResponseInputs []*proto.PostResponseInput

	pluginClient := &pipelinetest.PluginClient{
		PostResponseFunc: func(_ context.Context, in *proto.PostResponseInput) (*proto.PostResponseOutput, error) {
			postResponseInputs = append(postResponseInputs, in)
			return &proto.PostResponseOutput{}, nil
		},
	}

	plugins := pipelinetest.NewPluginProvider().
		Register("post-only", pipelinetest.PostOnlyDescriptor("post-only"), pluginClient)

	modelClient := &scriptedClient{responses: []*scriptedResponse{
		{content: "hello back"},
	}}
	resolver := pipelinetest.NewModelResolver().WithModel("org/gpt", modelClient)

	graph := pipelinetest.NewGraph().
		Generator("gen").
		Plugin("post", "post-only").
		ModelWithProxy("mdl", "org/gpt").
		Sink("sink").
		Edge("gen", "request", "post", "request").
		Edge("post", "request", "mdl", "request").
		Edge("mdl", "response", "sink", "response").
		Build()

	h := pipelinetest.New(
		pipelinetest.WithPlugins(plugins),
		pipelinetest.WithModelResolver(resolver),
	)

	result, err := h.Run(context.Background(), graph, pipelinetest.NewExecutionContext())
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}

	if result.Forward == nil {
		t.Fatal("expected a forward execution, got nil")
	}
	if result.Forward.ResolvedModel != "org/gpt" {
		t.Fatalf("forward pass: ResolvedModel = %q, want %q", result.Forward.ResolvedModel, "org/gpt")
	}
	if len(postResponseInputs) != 1 {
		t.Fatalf("expected exactly 1 PostResponse invocation, got %d", len(postResponseInputs))
	}
	gotModel := postResponseInputs[0].GetModel()
	if gotModel != "org/gpt" {
		t.Errorf("PostResponseInput.Model = %q, want %q (the model the forward pass resolved)", gotModel, "org/gpt")
	}
}
