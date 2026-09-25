package proxy

import (
	"context"
	"testing"

	genaiProxy "github.com/bornholm/genai/proxy"
	"github.com/xolo-gateway/xolo/internal/core/model"
	authn "github.com/xolo-gateway/xolo/internal/http/middleware/authn"
)

// populateMetaFromContext is what wires the quota enforcer to the
// application-scope check (issue #64). A bug here would silently re-open the
// gap: the user check would pass, the application check would not run, and
// the request would slip through. Pin the contract directly.

// Branch 1: XoloAuthExtractor's explicit context keys (orgID + applicationID)
// land in req.Metadata so a later PreRequestHook can read them. OrgID takes
// precedence over AuthTokenID in the keys this function copies.
func TestPopulateMetaFromContext_CopiesFromXoloAuthExtractorContext(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, contextKeyOrgID, "org-1")
	ctx = context.WithValue(ctx, contextKeyAuthTokenID, "tok-1")
	ctx = context.WithValue(ctx, contextKeyApplicationID, "app-1")

	req := &genaiProxy.ProxyRequest{Metadata: map[string]any{}}
	populateMetaFromContext(ctx, req)

	if got := OrgIDFromMeta(req.Metadata); got != model.OrgID("org-1") {
		t.Errorf("org id = %q, want org-1", got)
	}
	if got := AuthTokenIDFromMeta(req.Metadata); got != "tok-1" {
		t.Errorf("auth token id = %q, want tok-1", got)
	}
	if got := ApplicationIDFromMeta(req.Metadata); got != model.ApplicationID("app-1") {
		t.Errorf("application id = %q, want app-1", got)
	}
}

// Branch 2: a context captured before XoloAuthExtractor ran carries the
// identity through authn.ContextUser. An application principal is signalled by
// Provider=="application" and the Subject then holds the ApplicationID — the
// same shape the bridge middleware uses to resolve the shadow user.
func TestPopulateMetaFromContext_DerivesApplicationIDFromAuthnContext(t *testing.T) {
	ctx := authn.SetContextUser(context.Background(), &authn.User{
		OrgID:    "org-1",
		TokenID:  "tok-1",
		Provider: model.ApplicationProvider,
		Subject:  "app-1",
	})

	req := &genaiProxy.ProxyRequest{Metadata: map[string]any{}}
	populateMetaFromContext(ctx, req)

	if got := ApplicationIDFromMeta(req.Metadata); got != model.ApplicationID("app-1") {
		t.Errorf("application id = %q, want app-1", got)
	}
}

// A non-application provider on the authn.ContextUser path must NOT set
// MetaApplicationID: only the application provider carries a Subject that is
// an ApplicationID. A human principal would otherwise be charged against a
// non-existent application counter on the running spend path.
func TestPopulateMetaFromContext_DoesNotConfuseNonApplicationProvider(t *testing.T) {
	ctx := authn.SetContextUser(context.Background(), &authn.User{
		OrgID:    "org-1",
		TokenID:  "tok-1",
		Provider: "oidc",
		Subject:  "user-42",
	})

	req := &genaiProxy.ProxyRequest{Metadata: map[string]any{}}
	populateMetaFromContext(ctx, req)

	if got := ApplicationIDFromMeta(req.Metadata); got != "" {
		t.Errorf("application id = %q, want empty for non-application provider", got)
	}
}

// Once Metadata already carries an OrgID (e.g. an earlier hook in the chain
// populated it) the function must short-circuit and never overwrite it. The
// application-scope copy in particular must not stomp a value already set by
// PipelineHookAdapter at priority 3.
func TestPopulateMetaFromContext_DoesNotOverwriteMetadata(t *testing.T) {
	ctx := authn.SetContextUser(context.Background(), &authn.User{
		OrgID:    "org-from-authn",
		Provider: model.ApplicationProvider,
		Subject:  "app-from-authn",
	})

	req := &genaiProxy.ProxyRequest{Metadata: map[string]any{
		MetaOrgID: "org-already-set",
	}}
	populateMetaFromContext(ctx, req)

	if got := OrgIDFromMeta(req.Metadata); got != model.OrgID("org-already-set") {
		t.Errorf("org id = %q, want the value already in metadata (org-already-set)", got)
	}
	if got := ApplicationIDFromMeta(req.Metadata); got != "" {
		t.Errorf("application id = %q, want empty when the early-return fires", got)
	}
}

// Symmetric to TestPopulateMetaFromContext_DoesNotOverwriteMetadata, but for
// the XoloAuthExtractor explicit-keys branch: when req.Metadata already
// carries MetaOrgID, an application id carried by the explicit context keys
// must not be copied. The contract is "any prior MetaOrgID short-circuits the
// whole function", regardless of which context-source the application id
// would otherwise come from.
func TestPopulateMetaFromContext_XoloAuthExtractorBranchRespectsEarlyReturn(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, contextKeyOrgID, "org-from-context")
	ctx = context.WithValue(ctx, contextKeyApplicationID, "app-from-context")

	req := &genaiProxy.ProxyRequest{Metadata: map[string]any{
		MetaOrgID: "org-already-set",
	}}
	populateMetaFromContext(ctx, req)

	if got := OrgIDFromMeta(req.Metadata); got != model.OrgID("org-already-set") {
		t.Errorf("org id = %q, want the value already in metadata (org-already-set)", got)
	}
	if got := ApplicationIDFromMeta(req.Metadata); got != "" {
		t.Errorf("application id = %q, want empty when the early-return fires on the explicit-keys branch", got)
	}
}

// TestPopulateMetaFromContext_EmptyContextDoesNotPanic pins the
// authn.ContextUser fallback: the previous code called authn.ContextUser
// which panics when no user is attached. The fallback is reachable on a
// route that bypasses authn (a misconfigured upstream, a pre-auth hook on
// a non-application route). With OptionalContextUser the function returns
// silently instead, leaving the metadata empty for the caller to handle.
func TestPopulateMetaFromContext_EmptyContextDoesNotPanic(t *testing.T) {
	req := &genaiProxy.ProxyRequest{Metadata: map[string]any{}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("populateMetaFromContext panicked on an empty context: %v", r)
		}
	}()

	populateMetaFromContext(context.Background(), req)

	if got := OrgIDFromMeta(req.Metadata); got != "" {
		t.Errorf("org id = %q, want empty on an empty context", got)
	}
	if got := ApplicationIDFromMeta(req.Metadata); got != "" {
		t.Errorf("application id = %q, want empty on an empty context", got)
	}
}
