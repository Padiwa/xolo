package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/core/rbac"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/handler/api"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn"
)

// TestHandleModels_ApplicationTokenScopesByAuthnOrgID is a regression test
// for the bug reported in
// https://github.com/xolo-gateway/xolo/issues/48.
//
// An application created through the web UI authenticates through a shadow
// user that has no organization membership. The /api/v1/models endpoint
// previously looked up that shadow user's memberships, found none, and
// answered 200 with an empty list — even though the same application token
// successfully proxies requests against the org's enabled models.
//
// The fix: when an authn.User is in the context (which the authn middleware
// always guarantees on API routes), prefer its OrgID — the same source the
// proxy path uses via XoloAuthExtractor — so the catalogue lists models the
// proxy path can actually serve.
func TestHandleModels_ApplicationTokenScopesByAuthnOrgID(t *testing.T) {
	org := model.NewOrganization(testTenantID, "acme", "ACME", "")

	// Shadow user backing the application. It has no membership: that is
	// the whole point of the bug being triggered.
	appID := model.ApplicationID("app-1")
	shadowUser := model.NewUser(
		testTenantID,
		model.ApplicationProvider,
		string(appID),
		"", "CI",
		true, "user",
	)

	llmModel := model.NewLLMModel(model.NewProviderID(), org.ID(), "gpt-4o", "openai/gpt-4o", "", 100, 200)

	orgStore := &fakeOrgStoreForModels{
		orgsByID: map[string]model.Organization{
			string(org.ID()): org,
		},
		memberships: map[string][]model.Membership{}, // shadow user: empty
	}

	providerStore := &fakeProviderStoreForModels{
		enabledModels: map[string][]model.LLMModel{
			string(org.ID()): {llmModel},
		},
	}

	virtualModelStore := &fakeVirtualModelStoreForModels{
		models: map[string][]model.VirtualModel{},
	}

	h := api.NewHandler(providerStore, orgStore, virtualModelStore, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	ctx := req.Context()

	// Authn middleware has stamped the authn.User that mirrors the
	// application's token: OrgID = the org the application belongs to.
	// /api/v1/models is NOT a proxy request, so XoloAuthExtractor never
	// runs and the proxy-side contextKeyOrgID stays empty.
	ctx = authn.SetContextUser(ctx, &authn.User{
		Provider: model.ApplicationProvider,
		Subject:  string(appID),
		OrgID:    string(org.ID()),
		TokenID:  "tok-1",
		TenantID: string(testTenantID),
	})
	// The memberships middleware would have stored an empty list for this
	// shadow user — mirror it so the fallback path is exercised when
	// present.
	ctx = httpCtx.SetUser(ctx, shadowUser)
	ctx = httpCtx.SetPermissionResolver(ctx, func(ctx context.Context, orgID model.OrgID) (rbac.PermissionSet, error) {
		return rbac.NewPermissionSet([]string{string(rbac.PermModelUseOrg)}, nil), nil
	})
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

	if len(resp.Data) != 1 {
		t.Fatalf("expected the org's enabled models to be returned for the application token, got %d entries: %s", len(resp.Data), rec.Body.String())
	}
	if resp.Data[0].ID != org.Slug()+"/"+llmModel.ProxyName() {
		t.Fatalf("expected model id %q, got %q", org.Slug()+"/"+llmModel.ProxyName(), resp.Data[0].ID)
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
