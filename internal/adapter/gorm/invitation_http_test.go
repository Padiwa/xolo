package gorm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xolo-gateway/xolo/internal/adapter/events"
	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/xolo-gateway/xolo/internal/core/rbac"
	"github.com/xolo-gateway/xolo/internal/core/service"
	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	"github.com/xolo-gateway/xolo/internal/http/handler/webui"
	"github.com/xolo-gateway/xolo/internal/http/middleware/authn"
	"github.com/xolo-gateway/xolo/internal/http/middleware/bridge"
	"github.com/xolo-gateway/xolo/internal/http/middleware/memberships"
)

func invitationHTTPHandler(f *invitationFixture) http.Handler {
	s := f.store
	return memberships.Middleware(s, s)(webui.NewHandler(
		nil, s, s, s, s, s, s, s, s, events.NewInviteStore(s, f.recorder), f.service, s, s, nil, nil, s, scopeTestSecretKey,
		nil, nil, nil, s, s, s, s, 100, 100,
	))
}
func invitationRequest(handler http.Handler, tenant model.Tenant, user model.User, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := httpCtx.SetTenant(request.Context(), tenant)
	if user != nil {
		ctx = httpCtx.SetUser(ctx, user)
	}
	ctx = httpCtx.SetBaseURL(ctx, "http://gateway.example.test")
	request = request.WithContext(ctx)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestInvitationHTTPIsolation(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		for _, scenario := range []string{"foreign tenant", "wrong recipient", "unknown invite", "inactive user", "inactive org", "foreign role", "revoked", "expired", "exhausted"} {
			t.Run(scenario, func(t *testing.T) {
				for _, admin := range []bool{false, true} {
					t.Run(map[bool]string{false: "user", true: "platform admin"}[admin], func(t *testing.T) {
						f := newInvitationFixture(t, store)
						f.user.SetRoles(model.PlatformRoleUser)
						if admin {
							f.user.SetRoles(model.PlatformRoleAdmin)
						}
						require.NoError(t, store.SaveUser(f.ctx, f.user))
						tenant, user := f.tenant, model.User(f.user)
						role := string(f.role.ID())
						var expires *time.Time
						want := http.StatusNotFound
						switch scenario {
						case "foreign tenant":
							tenant = f.foreignTenant
						case "wrong recipient":
							user = f.other
							if admin {
								f.other.SetRoles(model.PlatformRoleAdmin)
								require.NoError(t, store.SaveUser(f.ctx, f.other))
							}
						case "inactive user":
							f.user.SetActive(false)
							require.NoError(t, store.SaveUser(f.ctx, f.user))
							want = http.StatusForbidden
						case "inactive org":
							require.NoError(t, store.SaveOrg(f.ctx, model.UpdateOrganization(f.org, model.WithOrgActive(false))))
							want = http.StatusBadRequest
						case "foreign role":
							role = string(f.foreignRole.ID())
							want = http.StatusBadRequest
						case "revoked", "expired", "exhausted":
							want = http.StatusBadRequest
						}
						if scenario == "expired" {
							past := time.Now().Add(-time.Hour)
							expires = &past
						}
						// An inactive recipient may still act on a targeted invitation
						// (TestInvitationHTTPInactiveInvitee): only an open one is refused.
						inv := f.invite(t, scenario != "inactive user", role, expires, ptr(1))
						id := inv.ID()
						if scenario == "revoked" {
							require.NoError(t, store.RevokeInvite(f.ctx, id))
						}
						if scenario == "exhausted" {
							require.NoError(t, store.IncrementInviteUses(f.ctx, id))
						}
						if scenario == "unknown invite" {
							id = model.NewInviteTokenID()
						}
						before, err := store.GetInviteByID(f.ctx, inv.ID())
						require.NoError(t, err)
						handler := invitationHTTPHandler(f)
						for _, op := range []struct{ method, path string }{{"GET", "/join/" + string(id)}, {"POST", "/join/" + string(id)}, {"POST", "/no-org/invitations/" + string(id) + "/decline"}} {
							response := invitationRequest(handler, tenant, user, op.method, op.path, "")
							require.Equal(t, want, response.Code, response.Body.String())
							for _, secret := range []string{f.org.Name(), f.role.Name(), f.user.Email(), string(inv.ID())} {
								require.NotContains(t, response.Body.String(), secret)
							}
							require.Empty(t, response.Result().Cookies())
							require.Empty(t, response.Header().Get("Location"))
						}
						after, err := store.GetInviteByID(f.ctx, inv.ID())
						require.NoError(t, err)
						require.Equal(t, before, after)
						_, count, err := store.ListOrgMembers(f.ctx, f.org.ID(), port.ListOrgMembersOptions{})
						require.NoError(t, err)
						require.Zero(t, count)
						require.Empty(t, f.recorder.snapshot())
					})
				}
			})
		}
	})
}

func TestInvitationHTTPJoinAndDecline(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		f := newInvitationFixture(t, store)
		handler := invitationHTTPHandler(f)
		targeted := f.invite(t, true, "", nil, nil)
		path := "/join/" + string(targeted.ID())
		response := invitationRequest(handler, f.tenant, nil, "GET", path, "")
		require.Equal(t, 200, response.Code)
		require.Contains(t, response.Body.String(), "Se connecter")
		for _, secret := range []string{f.org.Name(), f.role.Name(), f.user.Email()} {
			require.NotContains(t, response.Body.String(), secret)
		}
		response = invitationRequest(handler, f.tenant, nil, "POST", path, "")
		require.Equal(t, 303, response.Code)
		require.Equal(t, "/auth/oidc/login", response.Header().Get("Location"))
		response = invitationRequest(handler, f.tenant, f.user, "GET", path, "")
		require.Equal(t, 200, response.Code)
		require.Contains(t, response.Body.String(), f.role.Name())
		response = invitationRequest(handler, f.tenant, f.user, "POST", path, "")
		require.Equal(t, 200, response.Code)
		require.Contains(t, response.Body.String(), f.role.Name())
		require.Contains(t, response.Body.String(), "Vous avez rejoint")
		second := f.invite(t, true, model.RoleOrgOwner, nil, nil)
		response = invitationRequest(handler, f.tenant, f.user, "POST", "/join/"+string(second.ID()), "")
		require.Equal(t, 200, response.Code)
		require.Contains(t, response.Body.String(), "déjà membre")
		require.Contains(t, response.Body.String(), f.role.Name())
		require.Len(t, f.recorder.snapshot(), 3)
		require.NoError(t, store.RemoveMember(f.ctx, mustInvitationMembership(t, f).ID()))
		open := f.invite(t, false, "", nil, nil)
		response = invitationRequest(handler, f.tenant, nil, "GET", "/join/"+string(open.ID()), "")
		require.Equal(t, 200, response.Code)
		require.Contains(t, response.Body.String(), f.org.Name())
		require.Contains(t, response.Body.String(), f.role.Name())
		response = invitationRequest(handler, f.tenant, f.user, "POST", "/no-org/invitations/"+string(open.ID())+"/decline", "")
		require.Equal(t, 303, response.Code)
		require.Equal(t, "http://gateway.example.test/no-org", response.Header().Get("Location"))
		cookies := response.Result().Cookies()
		require.Len(t, cookies, 1)
		require.Equal(t, "declined_invite_"+string(open.ID()), cookies[0].Name)
		require.Equal(t, "1", cookies[0].Value)
		require.Equal(t, "/", cookies[0].Path)
		require.Equal(t, 86400, cookies[0].MaxAge)
		after, err := store.GetInviteByID(f.ctx, open.ID())
		require.NoError(t, err)
		require.Zero(t, after.UsesCount())
		// Existing local cookies still hide entries on the no-organization page.
		response = invitationRequest(handler, f.tenant, f.user, "GET", "/no-org", "", &http.Cookie{Name: "declined_invite_" + string(second.ID()), Value: "1"})
		require.Equal(t, 200, response.Code)
		require.NotContains(t, response.Body.String(), string(second.ID()))
		response = invitationRequest(handler, f.tenant, f.user, "POST", "/no-org/invitations/"+string(second.ID())+"/decline", "")
		require.Equal(t, 303, response.Code)
		require.Empty(t, response.Result().Cookies())
		response = invitationRequest(handler, f.tenant, f.user, "GET", "/join/"+string(second.ID()), "")
		require.Equal(t, 404, response.Code)
		require.NotContains(t, response.Body.String(), f.org.Name())
	})
}

// With AUTO_CREATE_USERS=false and ACTIVE_BY_DEFAULT=false, a targeted
// invitation lets the bridge create the account, inactive. The recipient must
// still be able to see, accept and decline it: it is the reason the account
// exists. An open invitation names nobody and keeps requiring an active account.
func TestInvitationHTTPInactiveInvitee(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		f := newInvitationFixture(t, store)
		handler := bridge.Middleware(store, store, nil, bridge.Options{AutoCreateUsers: false, ActiveByDefault: false})(invitationHTTPHandler(f))
		// The claim differs from the invitation by case and stray whitespace:
		// the bridge exemption and the invitation service must agree on it.
		identity := &authn.User{Provider: "test", Subject: "newcomer", Email: " Newcomer@Example.test ", DisplayName: "Newcomer"}
		request := func(method, path string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, nil)
			ctx := authn.SetContextUser(httpCtx.SetTenant(r.Context(), f.tenant), identity)
			ctx = httpCtx.SetBaseURL(ctx, "http://gateway.example.test")
			ctx = httpCtx.SetCurrentURL(ctx, r.URL)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r.WithContext(ctx))
			return response
		}
		invite := func(email *string) model.InviteToken {
			inv := model.NewInviteToken(f.org.ID(), string(f.role.ID()), email, nil, nil, f.other.ID())
			require.NoError(t, store.CreateInvite(f.ctx, inv))
			return inv
		}
		accepted, declined, open := invite(ptr("newcomer@example.test")), invite(ptr("newcomer@example.test")), invite(nil)
		elsewhere := invite(ptr(f.other.Email()))

		response := request("GET", "/join/"+string(accepted.ID()))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), f.role.Name())
		user, err := store.GetUserByIdentity(f.ctx, f.tenant.ID(), "test", "newcomer")
		require.NoError(t, err)
		require.False(t, user.Active(), "an invitation must not override ActiveByDefault")

		response = request("POST", "/join/"+string(accepted.ID()))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "Vous avez rejoint")
		member, err := store.IsMember(f.ctx, user.ID(), f.org.ID())
		require.NoError(t, err)
		require.True(t, member)

		response = request("POST", "/no-org/invitations/"+string(declined.ID())+"/decline")
		require.Equal(t, http.StatusSeeOther, response.Code, response.Body.String())
		_, err = store.GetInviteByID(f.ctx, declined.ID())
		require.ErrorIs(t, err, port.ErrNotFound)

		for _, op := range []struct{ method, path string }{{"GET", "/join/" + string(open.ID())}, {"POST", "/join/" + string(open.ID())}, {"POST", "/no-org/invitations/" + string(open.ID()) + "/decline"}} {
			response := request(op.method, op.path)
			require.Equal(t, http.StatusForbidden, response.Code, op.path)
			require.Empty(t, response.Result().Cookies())
		}
		after, err := store.GetInviteByID(f.ctx, open.ID())
		require.NoError(t, err)
		require.Zero(t, after.UsesCount())

		// Being inactive widens nothing beyond the invitations addressed to the
		// account: someone else's targeted invitation stays not found.
		for _, op := range []struct{ method, path string }{{"GET", "/join/" + string(elsewhere.ID())}, {"POST", "/join/" + string(elsewhere.ID())}, {"POST", "/no-org/invitations/" + string(elsewhere.ID()) + "/decline"}} {
			response := request(op.method, op.path)
			require.Equal(t, http.StatusNotFound, response.Code, op.path)
			require.NotContains(t, response.Body.String(), f.org.Name())
		}
		_, err = store.GetInviteByID(f.ctx, elsewhere.ID())
		require.NoError(t, err)

		// The rest of the instance still waits for an administrator.
		response = request("GET", "/usage")
		require.Equal(t, http.StatusForbidden, response.Code)
	})
}

func mustInvitationMembership(t *testing.T, f *invitationFixture) model.Membership {
	t.Helper()
	m, err := f.store.GetUserOrgMembership(f.ctx, f.user.ID(), f.org.ID())
	require.NoError(t, err)
	return m
}

func TestInvitationHTTPPendingLists(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		f := newInvitationFixture(t, store)
		handler := invitationHTTPHandler(f)
		valid := f.invite(t, true, "", nil, nil)
		foreign := model.NewInviteToken(f.foreignOrg.ID(), string(f.foreignRole.ID()), ptr(f.user.Email()), nil, nil, f.foreignUser.ID())
		require.NoError(t, store.CreateInvite(f.ctx, foreign))
		past := time.Now().Add(-time.Hour)
		expired := f.invite(t, true, "", &past, nil)
		revoked := f.invite(t, true, "", nil, nil)
		require.NoError(t, store.RevokeInvite(f.ctx, revoked.ID()))
		exhausted := f.invite(t, true, "", nil, ptr(1))
		require.NoError(t, store.IncrementInviteUses(f.ctx, exhausted.ID()))
		wrong := model.NewInviteToken(f.org.ID(), string(f.role.ID()), ptr(f.other.Email()), nil, nil, f.other.ID())
		require.NoError(t, store.CreateInvite(f.ctx, wrong))
		inactiveOrg := model.NewOrganization(f.tenant.ID(), "inactive", "INACTIVE-ORG", "")
		require.NoError(t, store.CreateOrg(f.ctx, model.UpdateOrganization(inactiveOrg, model.WithOrgActive(false))))
		inactive := model.NewInviteToken(inactiveOrg.ID(), model.RoleMember, ptr(f.user.Email()), nil, nil, f.other.ID())
		require.NoError(t, store.CreateInvite(f.ctx, inactive))
		pending, err := store.ListPendingInvitesForEmail(f.ctx, f.tenant.ID(), f.user.Email())
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, valid.ID(), pending[0].ID())
		pending, err = store.ListPendingInvitesForEmail(f.ctx, f.tenant.ID(), strings.ToUpper(f.user.Email()))
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, valid.ID(), pending[0].ID())
		pending, err = store.ListPendingInvitesForEmail(f.ctx, "", f.user.Email())
		require.ErrorIs(t, err, port.ErrInvalid)
		require.Empty(t, pending)
		for _, path := range []string{"/profile/invitations", "/no-org"} {
			response := invitationRequest(handler, f.tenant, f.user, "GET", path, "")
			require.Equal(t, 200, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), string(valid.ID()))
			require.Contains(t, response.Body.String(), f.role.Name())
			for _, inv := range []model.InviteToken{foreign, expired, revoked, exhausted, wrong, inactive} {
				require.NotContains(t, response.Body.String(), string(inv.ID()))
			}
			require.NotContains(t, response.Body.String(), f.foreignOrg.Name())
		}
		// A cached identity may still carry an earlier email. Rechecking the
		// persisted recipient must hide that invitation's entire row, not only
		// its role label.
		require.NoError(t, store.SaveUser(f.ctx, invitationUserEmail{f.user, "changed@example.test"}))
		for _, path := range []string{"/profile/invitations", "/no-org"} {
			response := invitationRequest(handler, f.tenant, f.user, "GET", path, "")
			require.Equal(t, 200, response.Code)
			require.NotContains(t, response.Body.String(), string(valid.ID()))
			require.NotContains(t, response.Body.String(), f.org.Name())
		}
	})
}

type invitationUserEmail struct {
	model.User
	email string
}

func (u invitationUserEmail) Email() string { return u.email }

func TestInvitationHTTPCreation(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		f := newInvitationFixture(t, store)
		// A custom role holding only invites:write retains permission to choose any
		// role within this organization, including the builtin owner role.
		creatorRole := model.NewRole(f.org.ID(), "Invitation manager", "")
		creatorRole.SetPermissions([]string{string(rbac.PermInvitesWrite)})
		require.NoError(t, store.CreateRole(f.ctx, creatorRole))
		creator := model.NewMembership(f.other.ID(), f.org.ID())
		require.NoError(t, store.AddMember(f.ctx, creator))
		require.NoError(t, store.SetMembershipRoles(f.ctx, creator.ID(), []model.RoleID{creatorRole.ID()}))
		handler := invitationHTTPHandler(f)
		path := "/orgs/" + f.org.Slug() + "/admin/invites"
		for _, body := range []string{"role=" + string(f.foreignRole.ID()), "role=unknown", "invitee_email=invalid", "expires_at=invalid", "expires_at=2000-01-01", "max_uses=0", "max_uses=-1", "max_uses=1.5", "max_uses=x", "invitee_email=%zz"} {
			response := invitationRequest(handler, f.tenant, f.other, "POST", path, body)
			require.Equal(t, 400, response.Code, body+": "+response.Body.String())
		}
		require.Empty(t, f.recorder.snapshot())
		response := invitationRequest(handler, f.tenant, f.user, "POST", path, "role="+string(f.role.ID()))
		require.Equal(t, 403, response.Code)
		response = invitationRequest(handler, f.tenant, f.other, "POST", path, "role=org%3Aowner&invitee_email="+url.QueryEscape(f.user.Email())+"&max_uses=2")
		require.Equal(t, 303, response.Code, response.Body.String())
		location, err := url.Parse(response.Header().Get("Location"))
		require.NoError(t, err)
		require.Equal(t, "created", location.Query().Get("success"))
		require.Contains(t, location.Query().Get("new_url"), "/join/")
		invites, err := store.ListInvites(f.ctx, f.org.ID())
		require.NoError(t, err)
		require.Len(t, invites, 1)
		require.Equal(t, 1, *invites[0].MaxUses())
		resolved, err := store.GetRoleByID(f.ctx, model.RoleID(invites[0].Role()))
		require.NoError(t, err)
		require.Equal(t, model.BuiltinKindOwner, resolved.BuiltinKind())
		require.Len(t, f.recorder.snapshot(), 1)
	})
}

type unreadableInvitationTx struct{ port.InvitationTx }

func (tx unreadableInvitationTx) GetInviteByID(context.Context, model.InviteTokenID) (model.InviteToken, error) {
	return nil, errInvitationInjected
}
func TestInvitationHTTPTechnicalErrors(t *testing.T) {
	eachBackend(t, func(t *testing.T, store *xologorm.Store) {
		f := newInvitationFixture(t, store)
		inv := f.invite(t, true, "", nil, nil)
		f.service = service.NewInvitationService(invitationTxWrapper{store, func(tx port.InvitationTx) port.InvitationTx { return unreadableInvitationTx{tx} }})
		handler := invitationHTTPHandler(f)
		for _, op := range []struct{ method, path string }{{"GET", "/join/" + string(inv.ID())}, {"POST", "/join/" + string(inv.ID())}, {"POST", "/no-org/invitations/" + string(inv.ID()) + "/decline"}} {
			response := invitationRequest(handler, f.tenant, f.user, op.method, op.path, "")
			require.Equal(t, 500, response.Code)
			require.Equal(t, "Internal Server Error\n", response.Body.String())
			require.Empty(t, response.Header().Get("Location"))
			require.Empty(t, response.Result().Cookies())
		}
		require.Empty(t, f.recorder.snapshot())
	})
}
