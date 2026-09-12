package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/markbates/goth"
	"github.com/markbates/goth/providers/openidConnect"
	"github.com/xolo-gateway/xolo/internal/config"
)

func TestOIDCCallbackURL(t *testing.T) {
	for name, baseURL := range map[string]string{
		"without trailing slash": "https://xolo.example.com/gateway",
		"with trailing slash":    "https://xolo.example.com/gateway/",
	} {
		t.Run(name, func(t *testing.T) {
			got := oidcCallbackURL(baseURL, "acme-idp")
			want := "https://xolo.example.com/gateway/auth/oidc/providers/acme-idp/callback"
			if got != want {
				t.Errorf("callback url: got %q, want %q", got, want)
			}
		})
	}
}

func TestBuildOIDCProviderDiscoversOnce(t *testing.T) {
	var requests atomic.Int32

	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{
			"issuer":"https://idp.example.com",
			"authorization_endpoint":"https://idp.example.com/authorize",
			"token_endpoint":"https://idp.example.com/token",
			"jwks_uri":"https://idp.example.com/jwks",
			"userinfo_endpoint":"https://idp.example.com/userinfo",
			"end_session_endpoint":"https://idp.example.com/logout"
		}`))
		if err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	t.Cleanup(discoveryServer.Close)

	factory, _, _, err := buildOIDCProvider(
		context.Background(),
		newOIDCDiscoveryHTTPClient(),
		config.NamedOIDCProvider{
			ID: "acme-idp",
			OIDCProvider: config.OIDCProvider{
				OAuth2Provider: config.OAuth2Provider{
					Key:    "client-id",
					Secret: "client-secret",
				},
				DiscoveryURL: discoveryServer.URL,
			},
		},
	)
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}

	goth.ClearProviders()
	t.Cleanup(goth.ClearProviders)

	if err := buildStartupOIDCProviders(
		map[string]oidcProviderFactory{"acme-idp": factory},
		"https://xolo.example.com",
		false,
	); err != nil {
		t.Fatalf("validate startup provider: %v", err)
	}

	registry := newHostScopedProviders(map[string]oidcProviderFactory{"acme-idp": factory})
	for _, baseURL := range []string{
		"https://one.xolo.example.com",
		"https://two.xolo.example.com",
	} {
		if _, err := registry.Resolve("acme-idp", baseURL); err != nil {
			t.Fatalf("resolve provider for %q: %v", baseURL, err)
		}
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("discovery requests: got %d, want 1", got)
	}
}

func TestBuildOIDCProviderRejectsInvalidDiscovery(t *testing.T) {
	for name, testCase := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"non-success status": {
			status: http.StatusBadGateway,
			body:   `{}`,
			want:   "status 502",
		},
		"malformed document": {
			status: http.StatusOK,
			body:   `{`,
			want:   "unexpected EOF",
		},
		"missing required endpoint": {
			status: http.StatusOK,
			body:   `{"issuer":"https://idp.example.com"}`,
			want:   `missing "authorization_endpoint"`,
		},
		"relative required endpoint": {
			status: http.StatusOK,
			body: `{
				"issuer":"https://idp.example.com",
				"authorization_endpoint":"/authorize",
				"token_endpoint":"https://idp.example.com/token",
				"jwks_uri":"https://idp.example.com/jwks"
			}`,
			want: `field "authorization_endpoint" must be an absolute`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, err := w.Write([]byte(testCase.body))
				if err != nil {
					t.Errorf("write discovery response: %v", err)
				}
			}))
			t.Cleanup(discoveryServer.Close)

			_, _, _, err := buildOIDCProvider(
				context.Background(),
				newOIDCDiscoveryHTTPClient(),
				config.NamedOIDCProvider{
					ID: "broken",
					OIDCProvider: config.OIDCProvider{
						OAuth2Provider: config.OAuth2Provider{
							Key:    "client-id",
							Secret: "client-secret",
						},
						DiscoveryURL: discoveryServer.URL,
					},
				},
			)
			if err == nil {
				t.Fatalf("error: got nil, want one containing %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error: got %q, want one containing %q", err, testCase.want)
			}
		})
	}
}

func TestOIDCDiscoveryClientTimeout(t *testing.T) {
	if got := newOIDCDiscoveryHTTPClient().Timeout; got != 10*time.Second {
		t.Fatalf("production timeout: got %s, want %s", got, 10*time.Second)
	}

	discoveryServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(discoveryServer.Close)

	client := &http.Client{Timeout: 20 * time.Millisecond}
	started := time.Now()
	_, err := fetchOIDCDiscovery(context.Background(), client, discoveryServer.URL)
	if err == nil {
		t.Fatal("error: got nil, want a timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error: got %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("timeout was not bounded: request took %s", elapsed)
	}
}

func TestBuildStartupOIDCProvidersRegistration(t *testing.T) {
	newFactory := func(built *int, instance **stubGothProvider) oidcProviderFactory {
		return func(callbackURL string) (goth.Provider, error) {
			*built++
			provider := &stubGothProvider{callbackURL: callbackURL}
			*instance = provider
			return provider, nil
		}
	}

	t.Run("multi-tenant validates without registering", func(t *testing.T) {
		goth.ClearProviders()
		t.Cleanup(goth.ClearProviders)

		built := 0
		var instance *stubGothProvider
		err := buildStartupOIDCProviders(
			map[string]oidcProviderFactory{"acme-idp": newFactory(&built, &instance)},
			"https://xolo.example.com",
			false,
		)
		if err != nil {
			t.Fatalf("startup validation: %v", err)
		}
		if built != 1 {
			t.Errorf("providers built: got %d, want 1", built)
		}
		if _, err := goth.GetProvider("acme-idp"); err == nil {
			t.Fatal("provider was registered in multi-tenant mode")
		}
	})

	t.Run("single-tenant registers the validated instance", func(t *testing.T) {
		goth.ClearProviders()
		t.Cleanup(goth.ClearProviders)

		built := 0
		var instance *stubGothProvider
		err := buildStartupOIDCProviders(
			map[string]oidcProviderFactory{"acme-idp": newFactory(&built, &instance)},
			"https://xolo.example.com/",
			true,
		)
		if err != nil {
			t.Fatalf("startup registration: %v", err)
		}
		if built != 1 {
			t.Errorf("providers built: got %d, want 1", built)
		}

		registered, err := goth.GetProvider("acme-idp")
		if err != nil {
			t.Fatalf("get registered provider: %v", err)
		}
		if registered != instance {
			t.Errorf("registered provider: got %p, want validated instance %p", registered, instance)
		}
		if want := "https://xolo.example.com/auth/oidc/providers/acme-idp/callback"; instance.callbackURL != want {
			t.Errorf("callback url: got %q, want %q", instance.callbackURL, want)
		}
	})
}

func TestBuildOIDCProviderUsesDiscoveredEndpoints(t *testing.T) {
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(`{
			"issuer":"https://idp.example.com",
			"authorization_endpoint":"https://idp.example.com/authorize",
			"token_endpoint":"https://idp.example.com/token",
			"jwks_uri":"https://idp.example.com/jwks"
		}`))
		if err != nil {
			t.Errorf("write discovery response: %v", err)
		}
	}))
	t.Cleanup(discoveryServer.Close)

	factory, _, _, err := buildOIDCProvider(
		context.Background(),
		newOIDCDiscoveryHTTPClient(),
		config.NamedOIDCProvider{
			ID: "acme-idp",
			OIDCProvider: config.OIDCProvider{
				OAuth2Provider: config.OAuth2Provider{Key: "key", Secret: "secret"},
				DiscoveryURL:   discoveryServer.URL,
			},
		},
	)
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}

	provider, err := factory("https://acme.xolo.example.com/callback")
	if err != nil {
		t.Fatalf("instantiate provider: %v", err)
	}
	custom, ok := provider.(*openidConnect.Provider)
	if !ok {
		t.Fatalf("provider: got %T, want *openidConnect.Provider", provider)
	}
	if got := custom.OpenIDConfig.AuthEndpoint; got != "https://idp.example.com/authorize" {
		t.Errorf("authorization endpoint: got %q", got)
	}
	if got := custom.OpenIDConfig.TokenEndpoint; got != "https://idp.example.com/token" {
		t.Errorf("token endpoint: got %q", got)
	}
}
