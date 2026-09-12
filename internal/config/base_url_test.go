package config_test

import (
	"strings"
	"testing"

	"github.com/xolo-gateway/xolo/internal/config"
)

// secretKeyForTest is a syntactically valid XOLO_SECRET_KEY: the root Validate
// checks it first, so every case below has to get past it.
const secretKeyForTest = "0000000000000000000000000000000000000000000000000000000000000000"

func TestValidateMultitenantBaseURL(t *testing.T) {
	for name, testCase := range map[string]struct {
		baseURL      string
		multitenancy config.Multitenancy
		wantErr      string
	}{
		"relative base url is fine on a single-tenant instance": {
			baseURL:      "/",
			multitenancy: config.Multitenancy{Enabled: false, DefaultTenantSlug: "default"},
		},
		"absolute base url with multi-tenancy": {
			baseURL: "https://xolo.example.com",
			multitenancy: config.Multitenancy{
				Enabled:           true,
				HostPattern:       "{tenant}.xolo.example.com",
				DefaultTenantSlug: "default",
			},
		},
		"relative base url with multi-tenancy": {
			baseURL: "/",
			multitenancy: config.Multitenancy{
				Enabled:           true,
				HostPattern:       "{tenant}.xolo.example.com",
				DefaultTenantSlug: "default",
			},
			wantErr: "XOLO_HTTP_BASE_URL must be absolute",
		},
		"schemeless base url with multi-tenancy": {
			baseURL: "xolo.example.com",
			multitenancy: config.Multitenancy{
				Enabled:           true,
				HostPattern:       "{tenant}.xolo.example.com",
				DefaultTenantSlug: "default",
			},
			wantErr: "XOLO_HTTP_BASE_URL must be absolute",
		},
	} {
		t.Run(name, func(t *testing.T) {
			conf := config.Config{
				SecretKey:    secretKeyForTest,
				HTTP:         config.HTTP{BaseURL: testCase.baseURL},
				Multitenancy: testCase.multitenancy,
			}

			err := conf.Validate()

			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("error: got nil, want one containing %q", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("error: got %q, want one containing %q", err, testCase.wantErr)
			}
		})
	}
}
