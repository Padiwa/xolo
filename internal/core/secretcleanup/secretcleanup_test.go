package secretcleanup_test

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"testing"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/secretcleanup"
)

type fakeSecretStore struct {
	deletedNodes []string
	deleted      [][3]string
}

func (s *fakeSecretStore) GetSecret(ctx context.Context, orgID, pluginName, nodeID, key string) (string, bool, error) {
	return "", false, nil
}

func (s *fakeSecretStore) SetSecret(ctx context.Context, orgID, pluginName, nodeID, key, value string) error {
	return nil
}

func (s *fakeSecretStore) DeleteSecret(ctx context.Context, orgID, pluginName, nodeID, key string) error {
	return nil
}

func (s *fakeSecretStore) DeleteAllForNode(ctx context.Context, scopeID, pluginName, nodeID string) error {
	s.deletedNodes = append(s.deletedNodes, nodeID)
	s.deleted = append(s.deleted, [3]string{scopeID, pluginName, nodeID})
	return nil
}

func graphWithNodes(ids ...string) *model.PipelineGraph {
	g := &model.PipelineGraph{}
	for _, id := range ids {
		g.Nodes = append(g.Nodes, model.PipelineNode{ID: id, Type: model.NodeTypePlugin, Data: []byte(`{"pluginName":"test"}`)})
	}
	return g
}

func TestPruneRemovedNodes_DeletesOnlyRemovedNodes(t *testing.T) {
	store := &fakeSecretStore{}
	oldGraph := graphWithNodes("node-1", "node-2", "node-3")
	newGraph := graphWithNodes("node-1", "node-3")

	if err := secretcleanup.PruneRemovedNodes(context.Background(), store, "org-1", oldGraph, newGraph); err != nil {
		t.Fatalf("PruneRemovedNodes: %v", err)
	}

	if len(store.deletedNodes) != 1 || store.deletedNodes[0] != "node-2" {
		t.Errorf("expected only node-2 to be pruned, got %v", store.deletedNodes)
	}
}

func TestPruneRemovedNodes_NilNewGraph_PrunesAllOldNodes(t *testing.T) {
	store := &fakeSecretStore{}
	oldGraph := graphWithNodes("node-1", "node-2")

	if err := secretcleanup.PruneRemovedNodes(context.Background(), store, "org-1", oldGraph, nil); err != nil {
		t.Fatalf("PruneRemovedNodes: %v", err)
	}

	if len(store.deletedNodes) != 2 {
		t.Errorf("expected both nodes to be pruned when the whole virtual model is deleted, got %v", store.deletedNodes)
	}
}

func TestPruneRemovedNodes_NoChange_PrunesNothing(t *testing.T) {
	store := &fakeSecretStore{}
	graph := graphWithNodes("node-1", "node-2")

	if err := secretcleanup.PruneRemovedNodes(context.Background(), store, "org-1", graph, graph); err != nil {
		t.Fatalf("PruneRemovedNodes: %v", err)
	}

	if len(store.deletedNodes) != 0 {
		t.Errorf("expected no pruning when the graph is unchanged, got %v", store.deletedNodes)
	}
}

func TestPruneRemovedNodes_NilOldGraph_NoOp(t *testing.T) {
	store := &fakeSecretStore{}

	if err := secretcleanup.PruneRemovedNodes(context.Background(), store, "org-1", nil, graphWithNodes("node-1")); err != nil {
		t.Fatalf("PruneRemovedNodes: %v", err)
	}

	if len(store.deletedNodes) != 0 {
		t.Errorf("expected no pruning when there is no old graph, got %v", store.deletedNodes)
	}
}

func TestPruneRemovedNodes_PlacementChanges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		node       model.PipelineNode
		wantDelete bool
	}{
		{"same plugin", model.PipelineNode{ID: "node", Type: model.NodeTypePlugin, Data: []byte(`{"pluginName":"test"}`)}, false},
		{"different plugin", model.PipelineNode{ID: "node", Type: model.NodeTypePlugin, Data: []byte(`{"pluginName":"other"}`)}, true},
		{"builtin", model.PipelineNode{ID: "node", Type: model.NodeTypeModel}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeSecretStore{}
			old := graphWithNodes("node")
			old.Nodes = append(old.Nodes, model.PipelineNode{ID: "builtin", Type: model.NodeTypeModel})
			require.NoError(t, secretcleanup.PruneRemovedNodes(t.Context(), store, "~:user", old, &model.PipelineGraph{Nodes: []model.PipelineNode{tc.node}}))
			if tc.wantDelete {
				require.Equal(t, [][3]string{{"~:user", "test", "node"}}, store.deleted)
			} else {
				require.Empty(t, store.deleted)
			}
		})
	}
}

func TestPruneRemovedNodes_ValidatesBothGraphsBeforeDeletion(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `{}`, `{"pluginName":" "}`, `{"pluginName":1}`} {
		for _, badOld := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/old=%v", raw, badOld), func(t *testing.T) {
				store := &fakeSecretStore{}
				good := graphWithNodes("removed")
				bad := graphWithNodes("valid-first", "malformed")
				bad.Nodes[1].Data = []byte(raw)
				old, next := good, bad
				if badOld {
					old, next = bad, good
				}
				require.ErrorIs(t, secretcleanup.PruneRemovedNodes(t.Context(), store, "org", old, next), port.ErrInvalid)
				require.Empty(t, store.deleted)
			})
		}
	}
	store := &fakeSecretStore{}
	require.ErrorIs(t, secretcleanup.PruneRemovedNodes(t.Context(), store, " ", graphWithNodes("node"), nil), port.ErrInvalid)
	require.Empty(t, store.deleted)
}
