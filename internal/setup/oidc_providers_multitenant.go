package setup

import (
	"net/url"
	"sync"

	"github.com/markbates/goth"
	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn/oidc"
)

// hostScopedProviders registers one goth provider per (provider, hostname)
// pair, on demand. A goth provider freezes its redirect URI at construction, so
// a multi-tenant instance can not share a single instance across tenants: the
// login flow has to come back to the host it started on, or the session it
// opens — bound to both the hostname and the tenant — is unusable.
//
// Tenants come and go through the provisioning API, so the set of hostnames is
// not known at startup and the registry fills in as hosts are seen.
type hostScopedProviders struct {
	factories map[string]oidcProviderFactory

	mutex      sync.RWMutex
	registered map[string]struct{}
}

func newHostScopedProviders(factories map[string]oidcProviderFactory) *hostScopedProviders {
	return &hostScopedProviders{
		factories:  factories,
		registered: make(map[string]struct{}),
	}
}

// Resolve returns the goth name serving providerID on the given public base
// URL, registering the provider the first time that pair is seen.
func (p *hostScopedProviders) Resolve(providerID string, baseURL string) (string, error) {
	factory, ok := p.factories[providerID]
	if !ok {
		return "", errors.Wrapf(oidc.ErrProviderNotFound, "no oidc provider %q is configured", providerID)
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", errors.Wrapf(err, "could not parse base url %q", baseURL)
	}

	if parsed.Host == "" {
		return "", errors.Errorf("base url %q carries no host to build a callback from", baseURL)
	}

	name := oidc.HostScopedProviderName(providerID, parsed.Host)

	p.mutex.RLock()
	_, known := p.registered[name]
	p.mutex.RUnlock()

	if known {
		return name, nil
	}

	// Built outside the lock so constructing one host-scoped instance does not
	// serialize the logins of every other tenant. Two requests racing on the
	// same host both build one and the second instance is simply dropped below.
	provider, err := factory(oidcCallbackURL(baseURL, providerID))
	if err != nil {
		return "", errors.Wrapf(err, "could not build oidc provider %q for host %q", providerID, parsed.Host)
	}

	provider.SetName(name)

	p.mutex.Lock()
	defer p.mutex.Unlock()

	if _, known := p.registered[name]; known {
		return name, nil
	}

	goth.UseProviders(provider)
	p.registered[name] = struct{}{}

	return name, nil
}
