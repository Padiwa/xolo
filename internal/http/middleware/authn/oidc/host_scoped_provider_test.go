package oidc_test

import (
	"testing"

	"github.com/xolo-gateway/xolo/internal/http/middleware/authn/oidc"
)

func TestHostScopedProviderName(t *testing.T) {
	for name, testCase := range map[string]struct {
		providerID string
		host       string
		want       string
	}{
		"scoped to a host":       {providerID: "github", host: "acme.xolo.example.com", want: "github@acme.xolo.example.com"},
		"the port is part of it": {providerID: "github", host: "acme.xolo.localhost:3002", want: "github@acme.xolo.localhost:3002"},
		"no host, no scope":      {providerID: "github", host: "", want: "github"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := oidc.HostScopedProviderName(testCase.providerID, testCase.host); got != testCase.want {
				t.Errorf("name: got %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestBaseProviderID(t *testing.T) {
	for name, testCase := range map[string]struct {
		providerName string
		want         string
	}{
		"host scoped":       {providerName: "github@acme.xolo.example.com", want: "github"},
		"host with a port":  {providerName: "github@acme.xolo.localhost:3002", want: "github"},
		"not scoped":        {providerName: "github", want: "github"},
		"leading separator": {providerName: "@acme.xolo.example.com", want: "@acme.xolo.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := oidc.BaseProviderID(testCase.providerName); got != testCase.want {
				t.Errorf("id: got %q, want %q", got, testCase.want)
			}
		})
	}
}
