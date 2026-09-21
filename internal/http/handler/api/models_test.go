package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/core/rbac"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/handler/api"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn"
)

// TestHandleModels_ScopeResolution exercises every branch of the scope
// resolution in GET /api/v1/models so a future refactor that swaps the order
// or drops the membership fallback is caught by CI. The three branches are:
//
//  1. Token-authenticated principal (application OR user): authn.User.OrgID
//     is set. The catalogue is scoped to that single org — mirroring the
//     proxy path's behaviour. This is the regression case from
//     https://github.com/xolo-gateway/xolo/issues/48: an application's shadow
//     user has no memberships, so without the authn-OrgID fast path the
//     catalogue comes back empty even though the same token successfully
//     proxies against those very models.
//
//  2. OIDC session principal: authn.User.OrgID is empty by design (a human
//     authenticated through a browser session can belong to many orgs and
//     picks the active one in the UI). The catalogue falls back to the
//     union of memberships.
//
//  3. User token authenticated against an org the user also belongs to by
//     membership: the authn-OrgID fast path still wins. The other memberships
//     are intentionally ignored — the proxy path has always behaved that way.
func TestHandleModels_ScopeResolution(t *testing.T) {
	orgA := model.NewOrganization(testTenantID, "acme", "ACME", "")
	orgB := model.NewOrganization(testTenantID, "umbrella", "Umbrella", "")

	modelA := model.NewLLMModel(model.NewProviderID(), orgA.ID(), "gpt-4o", "openai/gpt-4o", "", 100, 200)
	modelB := model.NewLLMModel(model.NewProviderID(), orgB.ID(), "claude", "anthropic/claude", "", 100, 200)

	permissionResolver := func(ctx context.Context, orgID model.OrgID) (rbac.PermissionSet, error) {
		return rbac.NewPermissionSet([]string{string(rbac.PermModelUseOrg)}, nil), nil
	}

	type membershipRef struct {
		orgID model.OrgID
	}

	cases := []struct {
		name string

		// Principal shape
		provider    string
		subject     string
		authnOrgID  string // empty for OIDC sessions
		xoloUser    model.User
		memberships []membershipRef

		// Expected model IDs in the response
		wantModelIDs []string
	}{
		{
			// Branch 1: application token. Regression case for #48.
			name:       "application token scopes by authn OrgID even with no memberships",
			subject:    "app-1",
			authnOrgID: string(orgA.ID()),
			xoloUser: model.NewUser(
				testTenantID,
				model.ApplicationProvider,
				"app-1",
				"", "CI",
				true, "user",
			),
			memberships:  nil, // shadow user has no membership
			wantModelIDs: []string{orgA.Slug() + "/" + modelA.ProxyName()},
		},
		{
			// Branch 2: OIDC session (no OrgID). Memberships drive the scope.
			name:       "OIDC session user sees the union of their memberships",
			subject:    "human-1",
			authnOrgID: "", // OIDC sessions leave OrgID empty
			xoloUser: model.NewUser(
				testTenantID,
				"oidc",
				"human-1",
				"alice@example.com", "Alice",
				true, "user",
			),
			memberships: []membershipRef{
				{orgID: orgA.ID()},
				{orgID: orgB.ID()},
			},
			wantModelIDs: []string{
				orgA.Slug() + "/" + modelA.ProxyName(),
				orgB.Slug() + "/" + modelB.ProxyName(),
			},
		},
		{
			// Branch 3: user token with memberships in additional orgs.
			// The token's OrgID wins — matches the proxy path's behaviour.
			name:       "user token scopes by authn OrgID even when the user has other memberships",
			subject:    "human-2",
			authnOrgID: string(orgA.ID()),
			xoloUser: model.NewUser(
				testTenantID,
				"oidc",
				"human-2",
				"bob@example.com", "Bob",
				true, "user",
			),
			memberships: []membershipRef{
				{orgID: orgA.ID()},
				{orgID: orgB.ID()}, // would leak if the membership fallback ran
			},
			wantModelIDs: []string{orgA.Slug() + "/" + modelA.ProxyName()},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build per-test memberships keyed by userID so the resolver
			// returns exactly what the case declares.
			membershipsByUser := map[string][]model.Membership{}
			if len(tc.memberships) > 0 {
				var ms []model.Membership
				for _, m := range tc.memberships {
					ms = append(ms, model.NewMembership(tc.xoloUser.ID(), m.orgID))
				}
				membershipsByUser[string(tc.xoloUser.ID())] = ms
			}

			orgStore := &fakeOrgStoreForModels{
				orgsByID: map[string]model.Organization{
					string(orgA.ID()): orgA,
					string(orgB.ID()): orgB,
				},
				memberships: membershipsByUser,
			}
			providerStore := &fakeProviderStoreForModels{
				enabledModels: map[string][]model.LLMModel{
					string(orgA.ID()): {modelA},
					string(orgB.ID()): {modelB},
				},
			}
			virtualModelStore := &fakeVirtualModelStoreForModels{
				models: map[string][]model.VirtualModel{},
			}

			h := api.NewHandler(providerStore, orgStore, virtualModelStore, nil, nil, nil, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
			ctx := req.Context()

			// Authn middleware stamps authn.User.OrgID from the token; OIDC
			// sessions leave it empty. /api/v1/models is not a proxy request,
			// so XoloAuthExtractor never runs and the proxy-side context key
			// stays unset for every case here.
			ctx = authn.SetContextUser(ctx, &authn.User{
				Provider: tc.provider,
				Subject:  tc.subject,
				OrgID:    tc.authnOrgID,
				TokenID:  "tok",
				TenantID: string(testTenantID),
			})
			ctx = httpCtx.SetUser(ctx, tc.xoloUser)
			ctx = httpCtx.SetPermissionResolver(ctx, permissionResolver)
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			var resp struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			got := make([]string, 0, len(resp.Data))
			for _, m := range resp.Data {
				got = append(got, m.ID)
			}
			slices.Sort(got)
			want := slices.Clone(tc.wantModelIDs)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Fatalf("unexpected model set\nwant: %v\ngot:  %v", want, got)
			}
		})
	}
}

// fakeOrgStoreForModels implements the OrgStore methods handleModels calls:
// GetOrgByID and GetUserMemberships. Other OrgStore methods panic loudly if
// accidentally reached.
type fakeOrgStoreForModels struct {
	port.OrgStore
	orgsByID    map[string]model.Organization
	memberships map[string][]model.Membership
}

func (s *fakeOrgStoreForModels) GetOrgByID(ctx context.Context, id model.OrgID) (model.Organization, error) {
	org, ok := s.orgsByID[string(id)]
	if !ok {
		return nil, port.ErrNotFound
	}
	return org, nil
}

func (s *fakeOrgStoreForModels) GetUserMemberships(ctx context.Context, userID model.UserID) ([]model.Membership, error) {
	return s.memberships[string(userID)], nil
}

// fakeProviderStoreForModels exposes only the ProviderStore methods used by
// handleModels. The rest panic loudly.
type fakeProviderStoreForModels struct {
	port.ProviderStore
	enabledModels map[string][]model.LLMModel
}

func (s *fakeProviderStoreForModels) ListEnabledLLMModels(ctx context.Context, orgID model.OrgID) ([]model.LLMModel, error) {
	return s.enabledModels[string(orgID)], nil
}

// fakeVirtualModelStoreForModels exposes only ListVirtualModels; the rest
// panic loudly.
type fakeVirtualModelStoreForModels struct {
	port.VirtualModelStore
	models map[string][]model.VirtualModel
}

func (s *fakeVirtualModelStoreForModels) ListVirtualModels(ctx context.Context, orgID model.OrgID) ([]model.VirtualModel, error) {
	return s.models[string(orgID)], nil
}
