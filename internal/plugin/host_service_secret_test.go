package plugin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xolo-gateway/xolo/internal/core/port"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type secretTestStore struct {
	port.SecretStore
	values map[[4]string]string
	err    error
	calls  int
}

func (s *secretTestStore) GetSecret(_ context.Context, scope, plugin, node, key string) (string, bool, error) {
	s.calls++
	value, found := s.values[[4]string{scope, plugin, node, key}]
	return value, found, s.err
}
func (s *secretTestStore) SetSecret(_ context.Context, scope, plugin, node, key, value string) error {
	s.calls++
	s.values[[4]string{scope, plugin, node, key}] = value
	return s.err
}
func (s *secretTestStore) DeleteSecret(_ context.Context, scope, plugin, node, key string) error {
	s.calls++
	delete(s.values, [4]string{scope, plugin, node, key})
	return s.err
}

func TestSecretRPCValidation(t *testing.T) {
	store := &secretTestStore{}
	// Invalid encryption key too: validation must run before encryption or storage.
	host := NewXoloHostService(nil, nil, store, nil, "invalid")
	for i := range 4 {
		key := [4]string{"scope", "plugin", "node", "key"}
		key[i] = " \t\u2003"
		_, err := host.GetSecret(t.Context(), &proto.GetSecretRequest{ScopeId: key[0], PluginName: key[1], NodeId: key[2], Key: key[3]})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = host.SetSecret(t.Context(), &proto.SetSecretRequest{ScopeId: key[0], PluginName: key[1], NodeId: key[2], Key: key[3]})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = host.DeleteSecret(t.Context(), &proto.DeleteSecretRequest{ScopeId: key[0], PluginName: key[1], NodeId: key[2], Key: key[3]})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	_, err := host.GetSecret(t.Context(), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = host.SetSecret(t.Context(), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = host.DeleteSecret(t.Context(), nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, store.calls)
}

func TestSecretRPCEncryptionIsolationAndErrors(t *testing.T) {
	store := &secretTestStore{values: map[[4]string]string{}}
	host := NewXoloHostService(nil, nil, store, nil, strings.Repeat("ab", 32))
	ctx := t.Context()
	for _, scope := range []string{"org", "~:user"} {
		_, err := host.SetSecret(ctx, &proto.SetSecretRequest{ScopeId: scope, PluginName: "p", NodeId: "n", Key: "k", Value: "clear-" + scope})
		require.NoError(t, err)
		require.NotContains(t, store.values[[4]string{scope, "p", "n", "k"}], "clear-")
		got, err := host.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: scope, PluginName: "p", NodeId: "n", Key: "k"})
		require.NoError(t, err)
		require.True(t, got.Found)
		require.Equal(t, "clear-"+scope, got.Value)
	}
	got, err := host.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: "other", PluginName: "p", NodeId: "n", Key: "k"})
	require.NoError(t, err)
	require.False(t, got.Found)
	_, err = host.SetSecret(ctx, &proto.SetSecretRequest{ScopeId: "org", PluginName: "p", NodeId: "n", Key: "empty", Value: ""})
	require.NoError(t, err)
	got, err = host.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: "org", PluginName: "p", NodeId: "n", Key: "empty"})
	require.NoError(t, err)
	require.True(t, got.Found)
	require.Empty(t, got.Value)
	store.err = errors.New("technical failure including sensitive payload")
	_, err = host.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: "org", PluginName: "p", NodeId: "n", Key: "k"})
	require.Equal(t, codes.Internal, status.Code(err))
	require.NotContains(t, err.Error(), "sensitive")
	_, err = host.SetSecret(ctx, &proto.SetSecretRequest{ScopeId: "org", PluginName: "p", NodeId: "n", Key: "k"})
	require.Equal(t, codes.Internal, status.Code(err))
	require.NotContains(t, err.Error(), "sensitive")
	_, err = host.DeleteSecret(ctx, &proto.DeleteSecretRequest{ScopeId: "org", PluginName: "p", NodeId: "n", Key: "k"})
	require.Equal(t, codes.Internal, status.Code(err))
	require.NotContains(t, err.Error(), "sensitive")
	store.err = nil
	store.values[[4]string{"org", "p", "n", "k"}] = "corrupted-sensitive-ciphertext"
	_, err = host.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: "org", PluginName: "p", NodeId: "n", Key: "k"})
	require.Equal(t, codes.Internal, status.Code(err))
	require.NotContains(t, err.Error(), "sensitive")
}

// The plugin only ever sees a generic message, so the cause has to reach the
// operator through the host log, identified by owner and key but never by value.
func TestSecretRPCFailuresAreLogged(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	const (
		clear      = "clear-secret-value"
		ciphertext = "corrupted-ciphertext"
	)
	validKey := strings.Repeat("ab", 32)
	storeFailure := errors.New("store unavailable")
	ctx := t.Context()
	id := [4]string{"org-1", "plugin-1", "node-1", "key-1"}

	for _, tc := range []struct {
		operation, secretKey, cause string
		storeErr                    error
		stored                      string
		call                        func(*XoloHostService) error
	}{
		{operation: "get", secretKey: validKey, cause: storeFailure.Error(), storeErr: storeFailure, call: func(h *XoloHostService) error {
			_, err := h.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: id[0], PluginName: id[1], NodeId: id[2], Key: id[3]})
			return err
		}},
		{operation: "decrypt", secretKey: validKey, cause: "invalid ciphertext hex", stored: ciphertext, call: func(h *XoloHostService) error {
			_, err := h.GetSecret(ctx, &proto.GetSecretRequest{ScopeId: id[0], PluginName: id[1], NodeId: id[2], Key: id[3]})
			return err
		}},
		{operation: "encrypt", secretKey: "invalid", cause: "invalid key hex", call: func(h *XoloHostService) error {
			_, err := h.SetSecret(ctx, &proto.SetSecretRequest{ScopeId: id[0], PluginName: id[1], NodeId: id[2], Key: id[3], Value: clear})
			return err
		}},
		{operation: "set", secretKey: validKey, cause: storeFailure.Error(), storeErr: storeFailure, call: func(h *XoloHostService) error {
			_, err := h.SetSecret(ctx, &proto.SetSecretRequest{ScopeId: id[0], PluginName: id[1], NodeId: id[2], Key: id[3], Value: clear})
			return err
		}},
		{operation: "delete", secretKey: validKey, cause: storeFailure.Error(), storeErr: storeFailure, call: func(h *XoloHostService) error {
			_, err := h.DeleteSecret(ctx, &proto.DeleteSecretRequest{ScopeId: id[0], PluginName: id[1], NodeId: id[2], Key: id[3]})
			return err
		}},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			logs.Reset()
			store := &secretTestStore{values: map[[4]string]string{}, err: tc.storeErr}
			if tc.stored != "" {
				store.values[id] = tc.stored
			}
			err := tc.call(NewXoloHostService(nil, nil, store, nil, tc.secretKey))
			require.Equal(t, codes.Internal, status.Code(err))
			require.NotContains(t, err.Error(), tc.cause)

			logged := logs.String()
			require.Equal(t, 1, strings.Count(logged, "level=ERROR"), logged)
			for _, want := range []string{
				"operation=" + tc.operation, "scope=" + id[0], "plugin=" + id[1], "node=" + id[2], "key=" + id[3], tc.cause,
			} {
				require.Contains(t, logged, want)
			}
			require.NotContains(t, logged, clear)
			require.NotContains(t, logged, ciphertext)
		})
	}
}
