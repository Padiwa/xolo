package org

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xolo-gateway/xolo/internal/adapter/events"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
)

// Unimplemented methods panic: denied requests may only resolve parents, never
// list data, read spending, prune secrets or perform any other downstream work.
type scopeStore struct {
	port.OrgStore
	port.ProviderStore
	port.VirtualModelStore
	port.MiddlewareStore
	port.InviteStore
	port.QuotaStore
	org, resourceOrg model.Organization
	provider         model.Provider
	llm              model.LLMModel
	vm               model.VirtualModel
	mw               model.Middleware
	invite           model.InviteToken
	member           model.Membership
	failAt           string
	err              error
	effects          []string
}

func (s *scopeStore) GetOrgBySlug(_ context.Context, tenant model.TenantID, slug string) (model.Organization, error) {
	if s.failAt == "slug" {
		return nil, s.err
	}
	if s.org.TenantID() != tenant || s.org.Slug() != slug {
		return nil, port.ErrNotFound
	}
	return s.org, nil
}
func (s *scopeStore) GetOrgByID(_ context.Context, id model.OrgID) (model.Organization, error) {
	if s.failAt == "invite parent" {
		return nil, s.err
	}
	if s.resourceOrg.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.resourceOrg, nil
}

func (s *scopeStore) GetProviderByID(_ context.Context, id model.ProviderID) (model.Provider, error) {
	if s.failAt == "provider" {
		return nil, s.err
	}
	if s.provider.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.provider, nil
}

func (s *scopeStore) GetLLMModelByID(_ context.Context, id model.LLMModelID) (model.LLMModel, error) {
	if s.failAt == "model" {
		return nil, s.err
	}
	if s.llm.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.llm, nil
}

func (s *scopeStore) GetVirtualModelByID(_ context.Context, id model.VirtualModelID) (model.VirtualModel, error) {
	if s.failAt == "resource" {
		return nil, s.err
	}
	if s.vm.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.vm, nil
}

func (s *scopeStore) GetMiddlewareByID(_ context.Context, id model.MiddlewareID) (model.Middleware, error) {
	if s.failAt == "resource" {
		return nil, s.err
	}
	if s.mw.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.mw, nil
}

func (s *scopeStore) GetInviteByID(_ context.Context, id model.InviteTokenID) (model.InviteToken, error) {
	if s.failAt == "resource" {
		return nil, s.err
	}
	if s.invite.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.invite, nil
}

func (s *scopeStore) GetMembership(_ context.Context, id model.MembershipID) (model.Membership, error) {
	if s.failAt == "resource" {
		return nil, s.err
	}
	if s.member.ID() != id {
		return nil, port.ErrNotFound
	}
	return s.member, nil
}

func (s *scopeStore) CreateProvider(context.Context, model.Provider) error {
	s.effects = append(s.effects, "CreateProvider")
	return nil
}

func (s *scopeStore) SaveProvider(context.Context, model.Provider) error {
	s.effects = append(s.effects, "SaveProvider")
	return nil
}

func (s *scopeStore) DeleteProvider(context.Context, model.ProviderID) error {
	s.effects = append(s.effects, "DeleteProvider")
	return nil
}

func (s *scopeStore) CreateLLMModel(context.Context, model.LLMModel) error {
	s.effects = append(s.effects, "CreateLLMModel")
	return nil
}

func (s *scopeStore) SaveLLMModel(context.Context, model.LLMModel) error {
	s.effects = append(s.effects, "SaveLLMModel")
	return nil
}

func (s *scopeStore) DeleteLLMModel(context.Context, model.LLMModelID) error {
	s.effects = append(s.effects, "DeleteLLMModel")
	return nil
}

func (s *scopeStore) SaveVirtualModel(context.Context, model.VirtualModel) error {
	s.effects = append(s.effects, "SaveVirtualModel")
	return nil
}

func (s *scopeStore) DeleteVirtualModel(context.Context, model.VirtualModelID) error {
	s.effects = append(s.effects, "DeleteVirtualModel")
	return nil
}

func (s *scopeStore) SaveMiddleware(context.Context, model.Middleware) error {
	s.effects = append(s.effects, "SaveMiddleware")
	return nil
}

func (s *scopeStore) DeleteMiddleware(context.Context, model.MiddlewareID) error {
	s.effects = append(s.effects, "DeleteMiddleware")
	return nil
}

func (s *scopeStore) DeleteInvite(context.Context, model.InviteTokenID) error {
	s.effects = append(s.effects, "DeleteInvite")
	return nil
}

func (s *scopeStore) RevokeInvite(context.Context, model.InviteTokenID) error {
	s.effects = append(s.effects, "RevokeInvite")
	return nil
}

func (s *scopeStore) SetQuota(context.Context, model.Quota) error {
	s.effects = append(s.effects, "SetQuota")
	return nil
}

func (s *scopeStore) GetQuota(context.Context, model.QuotaScope, string) (model.Quota, error) {
	s.effects = append(s.effects, "read quota")
	return nil, port.ErrNotFound
}
func (s *scopeStore) Emit(_ context.Context, _ model.Event) { s.effects = append(s.effects, "event") }

type scopeProvider struct {
	model.Provider
	store *scopeStore
}

func (p scopeProvider) APIKey() string {
	p.store.effects = append(p.store.effects, "access encrypted key")
	return "invalid ciphertext"
}

func TestResourceHandlersIsolation(t *testing.T) {
	tenant := model.NewTenant("a", "A", "")
	otherTenant := model.NewTenant("b", "B", "")
	own := model.NewOrganization(tenant.ID(), "shared", "Own", "")
	other := model.NewOrganization(tenant.ID(), "other", "Foreign", "")
	crossTenant := model.NewOrganization(otherTenant.ID(), own.Slug(), "Foreign tenant", "")
	backendErr := fmt.Errorf("wrapped: %w", errors.New("private database failure"))
	operations := []struct {
		name, family, method string
		invoke               func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"provider edit", "provider", "GET", (*Handler).getEditProviderPage},
		{"provider update", "provider", "POST", (*Handler).updateProvider},
		{"provider delete", "provider", "DELETE", (*Handler).deleteProvider},
		{"provider test", "provider", "POST", (*Handler).testProvider},
		{"models list", "provider", "GET", (*Handler).getModelsPage},
		{"model new", "provider", "GET", (*Handler).getNewModelPage},
		{"model create", "provider", "POST", (*Handler).createModel},
		{"model edit", "model", "GET", (*Handler).getEditModelPage},
		{"model update", "model", "POST", (*Handler).updateModel},
		{"model delete", "model", "DELETE", (*Handler).deleteModel},
		{"virtual edit", "virtual", "GET", (*Handler).getEditVirtualModelPage},
		{"virtual update", "virtual", "POST", (*Handler).updateVirtualModel},
		{"virtual delete", "virtual", "DELETE", (*Handler).deleteVirtualModel},
		{"virtual pipeline", "virtual", "GET", (*Handler).getPipelineEditorPage},
		{"middleware edit", "middleware", "GET", (*Handler).getEditMiddlewarePage},
		{"middleware update", "middleware", "POST", (*Handler).updateMiddleware},
		{"middleware toggle", "middleware", "POST", (*Handler).toggleMiddleware},
		{"middleware delete", "middleware", "DELETE", (*Handler).deleteMiddleware},
		{"middleware pipeline", "middleware", "GET", (*Handler).getMiddlewarePipelineEditorPage},
		{"invite revoke", "invite", "DELETE", (*Handler).revokeInvite},
		{"invite delete", "invite", "DELETE", (*Handler).deleteInvite},
		{"quota read", "quota", "GET", (*Handler).getMemberQuotaPage},
		{"quota save", "quota", "POST", (*Handler).saveMemberQuota},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			scenarios := []string{"same tenant", "other tenant same slug", "unknown", "lookup error", "missing org", "org error"}
			if op.family == "model" {
				scenarios = append(scenarios, "wrong provider", "wrong model org", "foreign provider", "provider error", "missing provider")
			}
			if op.family == "invite" {
				scenarios = append(scenarios, "missing invite parent", "invite parent error")
			}
			for _, scenario := range scenarios {
				t.Run(scenario, func(t *testing.T) {
					for _, admin := range []bool{false, true} {
						t.Run(fmt.Sprintf("admin=%v", admin), func(t *testing.T) {
							resourceOrg := own
							switch scenario {
							case "same tenant", "foreign provider", "wrong model org":
								resourceOrg = other
							case "other tenant same slug":
								resourceOrg = crossTenant
							}
							s := &scopeStore{org: own, resourceOrg: resourceOrg}
							providerOrg := resourceOrg
							if op.family == "model" && scenario != "foreign provider" {
								providerOrg = own
							}
							p := model.NewProvider(providerOrg.ID(), "PRIVATE-PROVIDER", "openai", "http://must-not-call.invalid", "ciphertext", "EUR")
							s.provider = scopeProvider{p, s}
							modelProviderID := p.ID()
							if scenario == "wrong provider" {
								modelProviderID = "another-provider-in-own-org"
							}
							s.llm = model.NewLLMModel(modelProviderID, resourceOrg.ID(), "PRIVATE-MODEL", "real", "", 0, 0)
							s.vm = model.NewVirtualModel(resourceOrg.ID(), "PRIVATE-VM", "")
							s.mw = model.NewMiddleware(resourceOrg.ID(), "PRIVATE-MIDDLEWARE", "")
							s.invite = model.NewInviteToken(resourceOrg.ID(), model.RoleMember, nil, nil, nil, "creator")
							s.member = model.NewMembership("PRIVATE-USER", resourceOrg.ID())
							want := http.StatusNotFound
							stage := "resource"
							if op.family == "provider" {
								stage = "provider"
							}
							if op.family == "model" {
								stage = "model"
							}
							switch scenario {
							case "lookup error":
								s.failAt, s.err, want = stage, backendErr, http.StatusInternalServerError
							case "unknown":
								s.failAt, s.err = stage, fmt.Errorf("wrapped: %w", port.ErrNotFound)
							case "missing org":
								s.failAt, s.err = "slug", port.ErrNotFound
							case "org error":
								s.failAt, s.err, want = "slug", backendErr, http.StatusInternalServerError
							case "provider error":
								s.failAt, s.err, want = "provider", backendErr, http.StatusInternalServerError
							case "missing provider":
								s.failAt, s.err = "provider", port.ErrNotFound
							case "missing invite parent":
								s.failAt, s.err = "invite parent", port.ErrNotFound
							case "invite parent error":
								s.failAt, s.err, want = "invite parent", backendErr, http.StatusInternalServerError
							}
							h := &Handler{
								orgStore:          s,
								providerStore:     events.NewProviderStore(s, s),
								virtualModelStore: events.NewVirtualModelStore(s, s),
								middlewareStore:   events.NewMiddlewareStore(s, s),
								inviteStore:       events.NewInviteStore(s, s),
								quotaStore:        s,
							}
							user := model.NewUser(tenant.ID(), "test", "actor", "actor@example.test", "Actor", true, model.PlatformRoleUser)
							if admin {
								user = model.NewUser(tenant.ID(), "test", "admin", "admin@example.test", "Admin", true, model.PlatformRoleAdmin)
							}
							req := httptest.NewRequest(op.method, "/", strings.NewReader("name=changed&enabled=on&daily_budget=10"))
							req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
							ctx := httpCtx.SetTenant(req.Context(), tenant)
							ctx = httpCtx.SetUser(ctx, user)
							req = req.WithContext(ctx)
							req.SetPathValue("orgSlug", own.Slug())
							req.SetPathValue("providerID", string(p.ID()))
							req.SetPathValue("modelID", string(s.llm.ID()))
							if op.family == "virtual" {
								req.SetPathValue("modelID", string(s.vm.ID()))
							}
							req.SetPathValue("middlewareID", string(s.mw.ID()))
							req.SetPathValue("inviteID", string(s.invite.ID()))
							req.SetPathValue("membershipID", string(s.member.ID()))
							// Exercise handlers directly so org lookup failures cannot be
							// intercepted by the permission middleware. Router/RBAC behavior
							// is covered separately by the real-store HTTP matrix.
							rr := httptest.NewRecorder()
							op.invoke(h, rr, req)
							if rr.Code != want || rr.Body.String() != http.StatusText(want)+"\n" {
								t.Fatalf("response = %d %q, want generic %d", rr.Code, rr.Body.String(), want)
							}
							if len(s.effects) != 0 {
								t.Fatalf("effects after denial: %v", s.effects)
							}
							if s.vm.Name() != "PRIVATE-VM" || s.mw.Name() != "PRIVATE-MIDDLEWARE" || !s.mw.Enabled() {
								t.Fatal("in-memory resource mutated before denial")
							}
						})
					}
				})
			}
		})
	}
}
