package setup

import (
	"errors"
	"strings"
	"testing"

	"github.com/markbates/goth"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn/oidc"
)

// stubGothProvider records the callback URL it was built with. Only Name and
// SetName are ever called on the registration path, so the embedded interface
// stays nil.
type stubGothProvider struct {
	goth.Provider
	name        string
	callbackURL string
}

func (p *stubGothProvider) Name() string {
	return p.name
}

func (p *stubGothProvider) SetName(name string) {
	p.name = name
}

func TestHostScopedProvidersResolve(t *testing.T) {
	goth.ClearProviders()
	t.Cleanup(goth.ClearProviders)

	newRegistry := func(built *int) *hostScopedProviders {
		return newHostScopedProviders(map[string]oidcProviderFactory{
			"acme-idp": func(callbackURL string) (goth.Provider, error) {
				*built++
				return &stubGothProvider{callbackURL: callbackURL}, nil
			},
		})
	}

	t.Run("binds the callback to the host of the request", func(t *testing.T) {
		built := 0
		registry := newRegistry(&built)

		name, err := registry.Resolve("acme-idp", "https://acme.xolo.example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := oidc.HostScopedProviderName("acme-idp", "acme.xolo.example.com")
		if name != want {
			t.Fatalf("name: got %q, want %q", name, want)
		}

		provider, err := goth.GetProvider(name)
		if err != nil {
			t.Fatalf("provider was not registered: %v", err)
		}

		stub, ok := provider.(*stubGothProvider)
		if !ok {
			t.Fatalf("provider: got %T, want *stubGothProvider", provider)
		}

		if wantCallback := "https://acme.xolo.example.com/auth/oidc/providers/acme-idp/callback"; stub.callbackURL != wantCallback {
			t.Errorf("callback url: got %q, want %q", stub.callbackURL, wantCallback)
		}
	})

	t.Run("builds one provider per host, once", func(t *testing.T) {
		built := 0
		registry := newRegistry(&built)

		for _, baseURL := range []string{
			"https://one.xolo.example.com",
			"https://one.xolo.example.com",
			"https://two.xolo.example.com",
		} {
			if _, err := registry.Resolve("acme-idp", baseURL); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		}

		if built != 2 {
			t.Errorf("providers built: got %d, want 2", built)
		}
	})

	t.Run("the name maps back to the configured provider id", func(t *testing.T) {
		built := 0
		registry := newRegistry(&built)

		name, err := registry.Resolve("acme-idp", "https://acme.xolo.example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// The host must not reach the identity stored in database: users are keyed
		// on (tenant, provider, subject).
		if got := oidc.BaseProviderID(name); got != "acme-idp" {
			t.Errorf("base provider id: got %q, want %q", got, "acme-idp")
		}
	})

	for name, testCase := range map[string]struct {
		providerID string
		baseURL    string
		wantErr    string
	}{
		"unknown provider": {
			providerID: "nope",
			baseURL:    "https://acme.xolo.example.com",
			wantErr:    `no oidc provider "nope" is configured`,
		},
		"base url without a host": {
			providerID: "acme-idp",
			baseURL:    "/",
			wantErr:    "carries no host",
		},
	} {
		t.Run(name, func(t *testing.T) {
			built := 0
			registry := newRegistry(&built)

			_, err := registry.Resolve(testCase.providerID, testCase.baseURL)
			if err == nil {
				t.Fatalf("error: got nil, want one containing %q", testCase.wantErr)
			}

			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("error: got %q, want one containing %q", err, testCase.wantErr)
			}
			if testCase.providerID == "nope" && !errors.Is(err, oidc.ErrProviderNotFound) {
				t.Errorf("error: got %v, want it to wrap ErrProviderNotFound", err)
			}
		})
	}
}
