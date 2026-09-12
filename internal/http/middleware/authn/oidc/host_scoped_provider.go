package oidc

import "strings"

// providerHostSeparator frames the hostname appended to a provider ID when the
// same provider is registered once per tenant host. It is not a legal character
// in a provider ID, which is a slug, so a name can always be split back.
const providerHostSeparator = "@"

// HostScopedProviderName is the name a provider is registered under when it is
// bound to a single hostname. A multi-tenant instance registers one goth
// provider per (provider, host) pair, because a provider carries its redirect
// URI and that URI has to point back at the host the login started on.
func HostScopedProviderName(providerID string, host string) string {
	if host == "" {
		return providerID
	}

	return providerID + providerHostSeparator + host
}

// BaseProviderID strips the host scope off a provider name. Identities are
// stored on (tenant, provider, subject), so the host must never reach the
// database: the same account signing in twice would otherwise be two users, and
// the unique identity index would stop catching it. It is also the ID the login
// and logout routes are mounted on.
func BaseProviderID(name string) string {
	if i := strings.LastIndex(name, providerHostSeparator); i > 0 {
		return name[:i]
	}

	return name
}
