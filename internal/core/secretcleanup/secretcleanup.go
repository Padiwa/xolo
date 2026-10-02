// Package secretcleanup removes secrets belonging to removed plugin placements.
package secretcleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

type placement struct{ plugin, node string }

// PruneRemovedNodes removes old (plugin, node) placements only within scopeID.
// A nil newGraph prunes the entire old graph. Both graphs are validated before
// any deletion, including when a node keeps its ID but changes plugin or type.
func PruneRemovedNodes(ctx context.Context, store port.SecretStore, scopeID string, oldGraph, newGraph *model.PipelineGraph) error {
	if strings.TrimSpace(scopeID) == "" {
		return fmt.Errorf("secret scope is required: %w", port.ErrInvalid)
	}
	old, err := placements(scopeID, oldGraph)
	if err != nil {
		return err
	}
	keep, err := placements(scopeID, newGraph)
	if err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	for p := range old {
		if _, ok := keep[p]; ok {
			continue
		}
		if err := store.DeleteAllForNode(ctx, scopeID, p.plugin, p.node); err != nil {
			return err
		}
	}
	return nil
}

func placements(scopeID string, graph *model.PipelineGraph) (map[placement]struct{}, error) {
	out := make(map[placement]struct{})
	if graph == nil {
		return out, nil
	}
	for _, node := range graph.Nodes {
		if node.Type != model.NodeTypePlugin {
			continue
		}
		var data model.PluginNodeData
		if err := json.Unmarshal(node.Data, &data); err != nil {
			return nil, fmt.Errorf("malformed plugin node %q: %w", node.ID, port.ErrInvalid)
		}
		if err := port.ValidateSecretScope(scopeID, data.PluginName, node.ID); err != nil {
			return nil, err
		}
		out[placement{data.PluginName, node.ID}] = struct{}{}
	}
	return out, nil
}
