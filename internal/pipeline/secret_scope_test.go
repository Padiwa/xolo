package pipeline_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/pipeline"
	"github.com/xolo-gateway/xolo/internal/pipeline/pipelinetest"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

type scopePersonalStore struct {
	port.PersonalVirtualModelStore
	vm model.PersonalVirtualModel
}

func (s scopePersonalStore) GetPersonalVirtualModelByName(_ context.Context, user model.UserID, name string) (model.PersonalVirtualModel, error) {
	if s.vm.UserID() == user && s.vm.Name() == name {
		return s.vm, nil
	}
	return nil, port.ErrNotFound
}

func scopeGraph(plugin, target string) *model.PipelineGraph {
	return pipelinetest.NewGraph().Generator("gen").Plugin("same-node", plugin).
		ModelWithProxy("model", target).Sink("sink").
		Edge("gen", "request", "same-node", "request").
		Edge("same-node", "request", "model", "request").
		Edge("model", "response", "sink", "response").Build()
}

func TestSecretScopeNestedGraphs(t *testing.T) {
	for _, org := range []string{"org-a", "org-b"} {
		t.Run(org, func(t *testing.T) {
			var scopes, backwardScopes []string
			client := &pipelinetest.PluginClient{PostResponseFunc: func(_ context.Context, in *proto.PostResponseInput) (*proto.PostResponseOutput, error) {
				backwardScopes = append(backwardScopes, in.Ctx.GetSecretScopeId())
				require.Equal(t, org, in.Ctx.GetOrgId())
				return &proto.PostResponseOutput{}, nil
			}, PreRequestFunc: func(_ context.Context, in *proto.PreRequestInput) (*proto.PreRequestOutput, error) {
				scopes = append(scopes, in.Ctx.SecretScopeId)
				require.Equal(t, org, in.Ctx.OrgId, "model, quota and event context must remain the organization")
				return &proto.PreRequestOutput{Allowed: true}, nil
			}}
			plugins := pipelinetest.NewPluginProvider().Register("probe", &proto.PluginDescriptor{
				Name: "probe", Capabilities: []proto.PluginDescriptor_Capability{proto.PluginDescriptor_PRE_REQUEST, proto.PluginDescriptor_POST_RESPONSE},
			}, client)
			personal := model.NewPersonalVirtualModel("user", "personal", "")
			personal.SetGraph(scopeGraph("probe", "org/inner"))
			inner := model.NewVirtualModel(model.OrgID(org), "inner", "")
			inner.SetGraph(scopeGraph("probe", "org/real"))
			h := pipelinetest.New(pipelinetest.WithPlugins(plugins), pipelinetest.WithVirtualModelStore(pipelinetest.NewVirtualModelStore(inner)))
			ec := pipelinetest.NewExecutionContext(pipelinetest.WithOrgID(org), pipelinetest.WithUserID("user"))
			ec.SecretScopeID = org
			ec.PersonalVMStore = scopePersonalStore{vm: personal}
			_, err := h.Run(t.Context(), scopeGraph("probe", "~/personal"), ec)
			require.NoError(t, err)
			require.Equal(t, []string{org, "~:user", org}, scopes)
			require.Equal(t, []string{org, "~:user", org}, backwardScopes)
			// Reusing the caller's context does not retain the nested personal scope.
			require.Equal(t, org, ec.SecretScopeID)
		})
	}
}

func TestSecretScopeToolContextIsRetained(t *testing.T) {
	var calls []*proto.RequestContext
	client := &pipelinetest.PluginClient{
		ListToolsFunc: func(_ context.Context, in *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
			calls = append(calls, in.Ctx)
			return &proto.ListToolsOutput{Tools: []*proto.ToolDescriptor{{Name: "tool"}}}, nil
		},
		CallToolFunc: func(_ context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
			calls = append(calls, in.Ctx)
			return &proto.CallToolOutput{}, nil
		},
		InspectToolResultFunc: func(_ context.Context, in *proto.InspectToolResultInput) (*proto.InspectToolResultOutput, error) {
			calls = append(calls, in.Ctx)
			return &proto.InspectToolResultOutput{}, nil
		},
	}
	plugins := pipelinetest.NewPluginProvider().Register("probe", &proto.PluginDescriptor{
		Name: "probe", Capabilities: []proto.PluginDescriptor_Capability{proto.PluginDescriptor_TOOL_PROVIDER, proto.PluginDescriptor_TOOL_RESULT_INSPECTOR},
	}, client)
	ec := pipeline.ExecutionContext{OrgID: "org", UserID: "user", SecretScopeID: "~:user", ToolInspectors: pipeline.NewToolInspectorSet()}
	node := model.PipelineNode{ID: "node", Type: model.NodeTypePlugin, Data: []byte(`{"pluginName":"probe"}`)}
	result, err := pipeline.NewPluginExecutor(plugins).Forward(t.Context(), node, nil, ec)
	require.NoError(t, err)
	ec.SecretScopeID = "org"
	_, err = result.Tools[0].Execute(t.Context(), map[string]any{})
	require.NoError(t, err)
	blocked, _ := ec.ToolInspectors.Inspect(t.Context(), "tool", "result")
	require.False(t, blocked)
	require.Len(t, calls, 3)
	for _, call := range calls {
		require.Equal(t, "~:user", call.SecretScopeId)
		require.Equal(t, "org", call.OrgId)
	}
}

func TestSecretScopeMiddlewareTransition(t *testing.T) {
	var received *proto.RequestContext
	client := &pipelinetest.PluginClient{PreRequestFunc: func(_ context.Context, in *proto.PreRequestInput) (*proto.PreRequestOutput, error) {
		received = in.Ctx
		return &proto.PreRequestOutput{Allowed: true}, nil
	}}
	plugins := pipelinetest.NewPluginProvider().Register("probe", &proto.PluginDescriptor{Name: "probe", Capabilities: []proto.PluginDescriptor_Capability{proto.PluginDescriptor_PRE_REQUEST}}, client)
	h := pipelinetest.New(pipelinetest.WithPlugins(plugins))
	mw := model.NewMiddleware("org", "middleware", "")
	mw.SetGraph(scopeGraph("probe", "ignored-under-middleware"))
	ec := pipeline.ExecutionContext{OrgID: "org", UserID: "user", SecretScopeID: "~:user", ForcePassthrough: true, TargetModelName: "org/real", PendingMiddlewares: []model.Middleware{mw}}
	executor := pipeline.NewModelExecutor(pipelinetest.NewModelResolver(), nil, h.Engine())
	_, err := executor.Forward(t.Context(), model.PipelineNode{ID: "next", Type: model.NodeTypeModel}, nil, ec)
	require.NoError(t, err)
	require.NotNil(t, received)
	require.Equal(t, "org", received.SecretScopeId)
	require.Equal(t, "org", received.OrgId)
	require.Equal(t, "~:user", ec.SecretScopeID)
}
