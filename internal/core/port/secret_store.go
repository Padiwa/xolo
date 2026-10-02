package port

import (
	"context"
	"fmt"
	"strings"
)

// SecretStore persists opaque values under (scopeID, pluginName, nodeID, key).
// Node IDs alone are not globally unique. Values are
// stored verbatim; callers (e.g. XoloHostService) are responsible for
// encrypting before SetSecret and decrypting after GetSecret, the same way
// Provider.APIKey is encrypted/decrypted by its callers rather than by the
// store. This backs the GetSecret/SetSecret/DeleteSecret RPCs exposed to
// plugins, so sensitive node configuration (e.g. an MCP server auth token)
// never has to be stored in the pipeline graph's visible JSON.
type SecretStore interface {
	GetSecret(ctx context.Context, scopeID, pluginName, nodeID, key string) (value string, found bool, err error)
	SetSecret(ctx context.Context, scopeID, pluginName, nodeID, key, value string) error
	DeleteSecret(ctx context.Context, scopeID, pluginName, nodeID, key string) error
	// DeleteAllForNode removes keys only for this scope, plugin and node.
	DeleteAllForNode(ctx context.Context, scopeID, pluginName, nodeID string) error
}

// ValidateSecretScope rejects missing components without normalizing identifiers.
// scopeID is an organization ID or "~:<userID>" for a personal profile.
func ValidateSecretScope(scopeID, pluginName, nodeID string) error {
	for i, value := range []string{scopeID, pluginName, nodeID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("secret %s is required: %w", []string{"scope", "plugin", "node"}[i], ErrInvalid)
		}
	}
	return nil
}

// ValidateSecretKey validates the full isolation key. Empty secret values are valid.
func ValidateSecretKey(scopeID, pluginName, nodeID, key string) error {
	if err := ValidateSecretScope(scopeID, pluginName, nodeID); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("secret key is required: %w", ErrInvalid)
	}
	return nil
}
