package setup

import (
	"net"
	"net/http"
	"net/url"

	"github.com/pkg/errors"
)

// newTenantBaseURLResolver derives the public base URL of a request from its
// own host, keeping the scheme and path of the configured base URL. A
// multi-tenant instance serves every tenant on its own hostname, so a single
// instance-wide base URL would send each of them links, redirects and OAuth
// callbacks pointing at another tenant's host.
//
// canonicalHost guards and normalizes the substitution: the Host header is
// client-controlled, so only the tenant slug is retained. The hostname is
// rebuilt from the configured tenant pattern, while the port always comes from
// baseURL. Such invalid hosts are answered 404 by the tenant middleware anyway
// — this only makes sure nothing is built from them in the meantime.
func newTenantBaseURLResolver(
	baseURL string,
	canonicalHost func(host string) (string, bool),
) (func(r *http.Request) string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.Wrapf(err, "could not parse base url %q", baseURL)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.Errorf("base url %q must be absolute (scheme and host) when multi-tenancy is enabled", baseURL)
	}

	return func(r *http.Request) string {
		host, ok := canonicalHost(r.Host)
		if !ok {
			return baseURL
		}

		perHost := *parsed
		perHost.Host = host
		if port := parsed.Port(); port != "" {
			perHost.Host = net.JoinHostPort(host, port)
		}

		return perHost.String()
	}, nil
}
