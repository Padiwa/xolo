package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xolo-gateway/xolo/internal/adapter/mcpclient"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

// newResourceMCPServer exposes the given resources and a single echo tool.
func newResourceMCPServer(t *testing.T, resources map[string]string) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echoes text"}, echo)

	for uri, text := range resources {
		server.AddResource(
			&mcp.Resource{URI: uri, Name: uri, MIMEType: "text/plain"},
			func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
					URI: uri, MIMEType: "text/plain", Text: text,
				}}}, nil
			},
		)
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		// A plugin under test keeps its MCP session open between requests; the
		// hanging notification stream it holds would make Close wait forever.
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

// newTestPlugin builds a plugin whose pooled sessions are closed when the test
// ends: keeping them open leaves the MCP server holding a stream, and the test's
// server teardown would wait on it.
func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{}
	p.SetHostClient(newFakeHostClient())
	t.Cleanup(p.Close)
	return p
}

func requestContext(endpoint, cfg string) *proto.RequestContext {
	return &proto.RequestContext{
		OrgId:         "org-1",
		SecretScopeId: "org-1",
		NodeId:        "node-1",
		UserId:        "user-1",
		SessionId:     "session-1",
		ConfigJson:    `{"endpoint":"` + endpoint + `"` + cfg + `}`,
	}
}

// TestPlugin_ConfiguredResourceIsInjected is the second acceptance criterion: a
// configured MCP resource shows up in the context, through the conversation
// values the host injects before the model's first call.
func TestPlugin_ConfiguredResourceIsInjected(t *testing.T) {
	srv := newResourceMCPServer(t, map[string]string{
		"file:///contexte/procedure.md": "Ouvrir un ticket avant tout redémarrage.",
	})

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, `,"resources":["file:///contexte/procedure.md"]`)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: reqCtx})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(out.ConversationValues) != 1 {
		t.Fatalf("expected the configured resource injected, got %v", out.ConversationValues)
	}
	value := out.ConversationValues[0]
	if value.GetContent() != "Ouvrir un ticket avant tout redémarrage." {
		t.Errorf("unexpected resource content: %q", value.GetContent())
	}
	if value.GetLabel() == "" {
		t.Error("expected the injected resource to carry a label")
	}
}

// TestPlugin_ResourceByServerName checks a resource named the way the server
// names it is resolved, like it is by URI.
func TestPlugin_ResourceByServerName(t *testing.T) {
	srv := newResourceMCPServer(t, map[string]string{"file:///readme.md": "the readme"})

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, `,"resources":["file:///readme.md"]`)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: reqCtx})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.ConversationValues) != 1 || out.ConversationValues[0].GetContent() != "the readme" {
		t.Errorf("expected the resource by URI, got %v", out.ConversationValues)
	}
}

// TestPlugin_MissingResourceIsReported makes sure a configured resource the
// server does not have does not fail the request.
func TestPlugin_MissingResourceIsReported(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, `,"resources":["file:///gone.md"]`)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: reqCtx})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.ConversationValues) != 1 {
		t.Fatalf("expected the missing resource reported, got %v", out.ConversationValues)
	}
	if out.ConversationValues[0].GetContent() == "" {
		t.Error("expected the missing resource reported in the injected text")
	}
}

// TestPlugin_ResourcesListedOnce makes sure several configured resources are
// resolved against a single resources/list: walking the whole catalogue once per
// reference would multiply the paginated listing on the request path.
func TestPlugin_ResourcesListedOnce(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	for _, uri := range []string{"file:///a.md", "file:///b.md", "file:///c.md"} {
		server.AddResource(
			&mcp.Resource{URI: uri, Name: uri, MIMEType: "text/plain"},
			func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
					URI: uri, MIMEType: "text/plain", Text: uri,
				}}}, nil
			},
		)
	}

	var lists atomic.Int32
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if bytes.Contains(body, []byte(`"resources/list"`)) {
				lists.Add(1)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, `,"resources":["file:///a.md","file:///b.md","file:///c.md"]`)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: reqCtx})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.ConversationValues) != 3 {
		t.Fatalf("expected the three resources injected, got %v", out.ConversationValues)
	}
	if n := lists.Load(); n != 1 {
		t.Errorf("expected a single resources/list for three references, got %d", n)
	}
}

// TestPlugin_PrefetchToolDeclared checks the configured prefetch tool is
// declared to the host, with the arguments template kept for the host to fill.
func TestPlugin_PrefetchToolDeclared(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, `,"prefetchTool":"echo","prefetchArguments":"{\"text\":\"{{last_user_message}}\"}","prefetchLabel":"Echo","prefetchResultTemplate":"[echo] {{result}}"`)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: reqCtx})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(out.ConversationTools) != 1 {
		t.Fatalf("expected one conversation tool, got %v", out.ConversationTools)
	}
	declared := out.ConversationTools[0]
	if declared.GetTrigger() != proto.ToolConversation_ALWAYS {
		t.Errorf("expected the ALWAYS trigger, got %v", declared.GetTrigger())
	}
	if declared.GetTool().GetName() != "echo" {
		t.Errorf("expected the echo tool, got %q", declared.GetTool().GetName())
	}
	if declared.GetArgumentsJson() != `{"text":"{{last_user_message}}"}` {
		t.Errorf("expected the arguments template kept verbatim, got %q", declared.GetArgumentsJson())
	}
	if declared.GetLabel() != "Echo" || declared.GetResultTemplate() != "[echo] {{result}}" {
		t.Errorf("unexpected label/template: %+v", declared)
	}
}

// TestPlugin_PrefetchToolOutsideFilterDropped makes sure the tool filter is not
// bypassed by a prefetch: a tool the administrator did not allow is not
// prefetched either.
func TestPlugin_PrefetchToolOutsideFilterDropped(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, `,"toolFilter":["other"],"prefetchTool":"echo"`)

	out, err := p.ListTools(context.Background(), &proto.ListToolsInput{Ctx: reqCtx})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(out.ConversationTools) != 0 {
		t.Errorf("expected no prefetch for a filtered-out tool, got %v", out.ConversationTools)
	}
}

// TestPlugin_OneSessionAcrossToolRounds checks the plugin resolves three tool
// rounds of one conversation on a single MCP session, which is what the
// connector is for.
func TestPlugin_OneSessionAcrossToolRounds(t *testing.T) {
	var sessions atomic.Int64
	seen := map[string]struct{}{}

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echoes text"}, echo)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("Mcp-Session-Id"); id != "" {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				sessions.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	// Cleanups run last registered first: the plugin closes the MCP sessions it
	// keeps open before the server waiting on them is torn down.
	t.Cleanup(srv.Close)

	p := newTestPlugin(t)
	reqCtx := requestContext(srv.URL, "")

	ctx := context.Background()
	if _, err := p.ListTools(ctx, &proto.ListToolsInput{Ctx: reqCtx}); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for round := range 3 {
		out, err := p.CallTool(ctx, &proto.CallToolInput{
			Ctx:           reqCtx,
			Name:          "echo",
			ArgumentsJson: `{"text":"round"}`,
		})
		if err != nil {
			t.Fatalf("round %d: CallTool: %v", round, err)
		}
		if out.GetResultText() != "echo: round" {
			t.Errorf("round %d: unexpected result %q", round, out.GetResultText())
		}
	}

	if got := sessions.Load(); got != 1 {
		t.Errorf("expected a single MCP session for the conversation, the server saw %d", got)
	}
}

// TestPlugin_SeparateConversationsSeparateSessions checks two conversations do
// not share a session: the state one leaves behind is not the other's.
func TestPlugin_SeparateConversationsSeparateSessions(t *testing.T) {
	var sessions atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echoes text"}, echo)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") == "" {
			sessions.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	p := newTestPlugin(t)

	first := requestContext(srv.URL, "")
	second := requestContext(srv.URL, "")
	second.SessionId = "session-2"

	for _, reqCtx := range []*proto.RequestContext{first, second} {
		if _, err := p.CallTool(context.Background(), &proto.CallToolInput{
			Ctx:           reqCtx,
			Name:          "echo",
			ArgumentsJson: `{"text":"hi"}`,
		}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	if got := sessions.Load(); got != 2 {
		t.Errorf("expected one session per conversation (2), saw %d", got)
	}
}

// countingAuthServer echoes back the Authorization header it received, so a test
// can tell which credential a pooled session is actually sending.
func newEchoAuthServer(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echoes the auth header"}, func(_ context.Context, _ *mcp.CallToolRequest, _ echoInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})

	var mu sync.Mutex
	var last string
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			mu.Lock()
			last = auth
			mu.Unlock()
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// TestPlugin_PooledConnectorPicksUpRotatedSecret is the review's first finding:
// SetConfig was never called, so a pooled connector kept the credential it was
// created with. A rotated secret must reach the session.
func TestPlugin_PooledConnectorPicksUpRotatedSecret(t *testing.T) {
	srv, lastAuth := newEchoAuthServer(t)

	p := newTestPlugin(t)
	ctx := context.Background()
	// The header name is what makes the stored secret be sent at all.
	reqCtx := requestContext(srv.URL, `,"authHeaderName":"Authorization"`)

	call := func() {
		t.Helper()
		if _, err := p.CallTool(ctx, &proto.CallToolInput{
			Ctx:           reqCtx,
			Name:          "echo",
			ArgumentsJson: `{"text":"hi"}`,
		}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	// The secret is read from the host store on every call.
	setSecret := func(value string) {
		t.Helper()
		if err := p.getHostClient().SetSecret(ctx, "org-1", "mcp-bridge", "node-1", secretKeyAuthValue, value); err != nil {
			t.Fatalf("SetSecret: %v", err)
		}
	}

	setSecret("Bearer first")
	call()
	if got := lastAuth(); got != "Bearer first" {
		t.Fatalf("expected the first secret to be sent, got %q", got)
	}

	setSecret("Bearer rotated")
	call()
	if got := lastAuth(); got != "Bearer rotated" {
		t.Errorf("expected the rotated secret on the same pooled session, got %q", got)
	}
}

// TestPlugin_PoolKeepsConnectorAcrossSessionReset pins the behaviour the second
// review found wrong: expiring the session must not evict the pooled connector,
// because Do resets the session on the ordinary dead-session-retry path and
// connector() refreshes credentials on every request.
func TestPlugin_PoolKeepsConnectorAcrossSessionReset(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	ctx := context.Background()
	reqCtx := requestContext(srv.URL, "")

	if _, err := p.CallTool(ctx, &proto.CallToolInput{
		Ctx:           reqCtx,
		Name:          "echo",
		ArgumentsJson: `{"text":"hi"}`,
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	p.mu.Lock()
	connectors := make([]*mcpclient.Connector, 0, len(p.sessions))
	for _, c := range p.sessions {
		connectors = append(connectors, c)
	}
	p.mu.Unlock()
	if len(connectors) != 1 {
		t.Fatalf("expected one pooled connector, got %d", len(connectors))
	}

	// Resetting the session is what the retry path does; the entry must stay,
	// or every retry would rebuild a connector and a session.
	connectors[0].Reset()

	p.mu.Lock()
	after := len(p.sessions)
	p.mu.Unlock()
	if after != 1 {
		t.Errorf("a session reset must keep the pooled connector, %d entries remain", after)
	}

	// Closing the connector does remove it: that is a real retirement.
	connectors[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		remaining := len(p.sessions)
		p.mu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a closed connector must leave the pool")
}

// TestPlugin_SessionKeyIncludesIdentity is the review's security finding: the
// session id comes from the client, so two callers sending the same value must
// still get two sessions rather than sharing one and its credentials.
func TestPlugin_SessionKeyIncludesIdentity(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	ctx := context.Background()

	first := requestContext(srv.URL, "")
	second := requestContext(srv.URL, "")
	second.UserId = "user-2" // same session id, different caller

	for _, reqCtx := range []*proto.RequestContext{first, second} {
		if _, err := p.CallTool(ctx, &proto.CallToolInput{
			Ctx:           reqCtx,
			Name:          "echo",
			ArgumentsJson: `{"text":"hi"}`,
		}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	p.mu.Lock()
	pooled := len(p.sessions)
	p.mu.Unlock()
	if pooled != 2 {
		t.Errorf("expected one session per caller despite the shared session id, got %d", pooled)
	}
}

// TestPlugin_PoolConcurrentFirstRequests exercises the pool under the condition
// that broke it: several requests hitting different conversations at once, so
// each builds a connector. Writing p.keys outside p.mu made this a fatal
// "concurrent map writes", which aborts the whole plugin process rather than
// one request. Run with -race to catch the regression.
func TestPlugin_PoolConcurrentFirstRequests(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	ctx := context.Background()

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A distinct session id per caller: distinct pool keys, all created
			// concurrently on a cold pool.
			reqCtx := requestContext(srv.URL, "")
			reqCtx.SessionId = fmt.Sprintf("session-%d", i)
			_, err := p.CallTool(ctx, &proto.CallToolInput{
				Ctx:           reqCtx,
				Name:          "echo",
				ArgumentsJson: `{"text":"hi"}`,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent CallTool: %v", err)
		}
	}

	p.mu.Lock()
	pooled := len(p.sessions)
	indexed := len(p.keys)
	p.mu.Unlock()
	if pooled != callers || indexed != callers {
		t.Errorf("expected %d sessions and %d keys, got %d and %d", callers, callers, pooled, indexed)
	}
}

// TestPlugin_NoSessionHeaderOpensOneSessionPerRequest pins the fallback the
// maintainer asked to change: a client that sends no conversation id must not
// have its separate conversations share one MCP session, since that session
// carries the server's per-conversation state.
func TestPlugin_NoSessionHeaderOpensOneSessionPerRequest(t *testing.T) {
	var sessions atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echoes text"}, echo)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})

	p := newTestPlugin(t)
	ctx := context.Background()

	// Two requests of the same caller, neither carrying a session id: they are
	// unrelated conversations and must not share a session.
	for range 2 {
		reqCtx := requestContext(srv.URL, "")
		reqCtx.SessionId = ""
		if _, err := p.CallTool(ctx, &proto.CallToolInput{
			Ctx:           reqCtx,
			Name:          "echo",
			ArgumentsJson: `{"text":"hi"}`,
		}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	p.mu.Lock()
	pooled := len(p.sessions)
	p.mu.Unlock()
	if pooled != 0 {
		t.Errorf("a request with no conversation id must not be pooled, %d entries in the pool", pooled)
	}
	_ = sessions.Load()
}

// TestPlugin_RetiredConnectorIsReplacedNotReturned is the review's high finding:
// a connector retired by its idle timer between the pool lookup and the call
// made the request fail with "connector closed", because retirement is terminal
// and SetConfig does not revive one. It must be a pool miss instead.
func TestPlugin_RetiredConnectorIsReplacedNotReturned(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	ctx := context.Background()
	reqCtx := requestContext(srv.URL, "")

	call := func() {
		t.Helper()
		if _, err := p.CallTool(ctx, &proto.CallToolInput{
			Ctx:           reqCtx,
			Name:          "echo",
			ArgumentsJson: `{"text":"hi"}`,
		}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	call()

	// Retire the pooled connector the way the idle timer does. Its pool entry is
	// removed asynchronously, so the next request can still find it.
	p.mu.Lock()
	connectors := make([]*mcpclient.Connector, 0, len(p.sessions))
	for _, c := range p.sessions {
		connectors = append(connectors, c)
	}
	p.mu.Unlock()
	if len(connectors) != 1 {
		t.Fatalf("expected one pooled connector, got %d", len(connectors))
	}
	connectors[0].Close()

	// Must open a fresh session rather than fail the request.
	call()
}

// TestPlugin_RebuildsOnErrConnectorClosed exercises the rebuild branch of
// doWithRebuild directly.
//
// Retiring the connector before a normal call() does not reach it: connector()
// re-checks Terminal() after SetConfig and treats a retired entry as a pool miss.
// The branch exists for the state the idle ticker leaves between connector()
// returning it and Do reaching acquire(), so the test drives that state by hand.
func TestPlugin_RebuildsOnErrConnectorClosed(t *testing.T) {
	srv := newResourceMCPServer(t, nil)

	p := newTestPlugin(t)
	ctx := context.Background()
	reqCtx := requestContext(srv.URL, "")

	// Populate the pool.
	if _, err := p.CallTool(ctx, &proto.CallToolInput{
		Ctx:           reqCtx,
		Name:          "echo",
		ArgumentsJson: `{"text":"hi"}`,
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	p.mu.Lock()
	var stale *mcpclient.Connector
	for _, c := range p.sessions {
		stale = c
	}
	p.mu.Unlock()
	if stale == nil {
		t.Fatal("no pooled connector")
	}

	// Retire it: this is the connector the pool hands out just before the ticker
	// wins the race, so Do reports ErrConnectorClosed.
	stale.Close()

	var runs int
	err := p.doWithRebuild(ctx, reqCtx, stale, true, func(session *mcp.ClientSession) error {
		runs++
		_, _, callErr := mcpclient.CallToolText(ctx, session, "echo", `{"text":"hi"}`)
		return callErr
	})
	if err != nil {
		t.Fatalf("doWithRebuild must rebuild instead of surfacing ErrConnectorClosed, got %v", err)
	}
	if runs != 1 {
		t.Errorf("expected fn to run once on the fresh connector, ran %d times", runs)
	}
}
