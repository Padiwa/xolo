package oidc

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/markbates/goth/gothic"
	"github.com/pkg/errors"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
)

// serveWithContextProvider runs the middleware for one provider route and
// reports the status and the provider name gothic would read from the context.
func serveWithContextProvider(
	t *testing.T,
	h *Handler,
	providerID string,
	baseURL string,
	rawQuery string,
) (int, string, url.Values) {
	t.Helper()

	var seen string
	seenQuery := url.Values{}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, err := gothic.GetProviderName(r)
		if err != nil {
			t.Errorf("gothic provider resolution: %v", err)
		}
		seen = name
		seenQuery = r.URL.Query()
	})

	target := "/providers/" + providerID
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("provider", providerID)
	req = req.WithContext(httpCtx.SetBaseURL(req.Context(), baseURL))

	rec := httptest.NewRecorder()
	h.withContextProvider(next).ServeHTTP(rec, req)

	return rec.Code, seen, seenQuery
}

func TestWithContextProvider(t *testing.T) {
	t.Run("without a resolver the route parameter is used as is", func(t *testing.T) {
		h := &Handler{}

		status, provider, _ := serveWithContextProvider(t, h, "github", "https://xolo.example.com", "")

		if status != http.StatusOK {
			t.Fatalf("status: got %d, want %d", status, http.StatusOK)
		}
		if provider != "github" {
			t.Errorf("provider: got %q, want %q", provider, "github")
		}
	})

	t.Run("the resolver names the provider serving this host", func(t *testing.T) {
		h := &Handler{
			resolveProvider: func(providerID string, baseURL string) (string, error) {
				if baseURL != "https://acme.xolo.example.com" {
					t.Errorf("base url: got %q, want the host of the request", baseURL)
				}

				return HostScopedProviderName(providerID, "acme.xolo.example.com"), nil
			},
		}

		status, provider, _ := serveWithContextProvider(t, h, "github", "https://acme.xolo.example.com", "")

		if status != http.StatusOK {
			t.Fatalf("status: got %d, want %d", status, http.StatusOK)
		}
		if want := "github@acme.xolo.example.com"; provider != want {
			t.Errorf("provider: got %q, want %q", provider, want)
		}
	})

	t.Run("a provider that can not be resolved is a 404", func(t *testing.T) {
		h := &Handler{
			resolveProvider: func(providerID string, _ string) (string, error) {
				return "", errors.Wrapf(ErrProviderNotFound, "no such provider %q", providerID)
			},
		}

		status, provider, _ := serveWithContextProvider(t, h, "nope", "https://acme.xolo.example.com", "")

		if status != http.StatusNotFound {
			t.Fatalf("status: got %d, want %d", status, http.StatusNotFound)
		}
		if provider != "" {
			t.Errorf("the request should not have been served, got provider %q", provider)
		}
	})

	t.Run("an operational resolver failure is logged once and rendered as a 502", func(t *testing.T) {
		var logs bytes.Buffer
		previousLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previousLogger) })

		h := &Handler{
			resolveProvider: func(string, string) (string, error) {
				return "", errors.New("provider construction failed")
			},
		}

		status, provider, _ := serveWithContextProvider(t, h, "broken", "https://acme.xolo.example.com", "")

		if status != http.StatusBadGateway {
			t.Fatalf("status: got %d, want %d", status, http.StatusBadGateway)
		}
		if provider != "" {
			t.Errorf("the request should not have been served, got provider %q", provider)
		}
		if got := strings.Count(logs.String(), "could not resolve oidc provider"); got != 1 {
			t.Errorf("resolution log entries: got %d, want 1; logs: %s", got, logs.String())
		}
	})

	for _, parameter := range []string{"provider", ":provider"} {
		t.Run("query parameter "+parameter+" cannot override the route", func(t *testing.T) {
			h := &Handler{
				resolveProvider: func(providerID string, _ string) (string, error) {
					return HostScopedProviderName(providerID, "acme.xolo.example.com"), nil
				},
			}

			query := url.Values{
				parameter:   {"attacker"},
				"code":      {"authorization-code"},
				"state":     {"csrf-state"},
				"unrelated": {"kept"},
			}
			status, provider, gotQuery := serveWithContextProvider(
				t,
				h,
				"github",
				"https://acme.xolo.example.com",
				query.Encode(),
			)

			if status != http.StatusOK {
				t.Fatalf("status: got %d, want %d", status, http.StatusOK)
			}
			if want := "github@acme.xolo.example.com"; provider != want {
				t.Errorf("gothic provider: got %q, want %q", provider, want)
			}
			if got := gotQuery.Get(parameter); got != "" {
				t.Errorf("query parameter %q was not removed: %q", parameter, got)
			}
			if got := gotQuery.Get("code"); got != "authorization-code" {
				t.Errorf("code: got %q, want %q", got, "authorization-code")
			}
			if got := gotQuery.Get("state"); got != "csrf-state" {
				t.Errorf("state: got %q, want %q", got, "csrf-state")
			}
			if got := gotQuery.Get("unrelated"); got != "kept" {
				t.Errorf("unrelated query parameter: got %q, want %q", got, "kept")
			}
		})
	}
}
