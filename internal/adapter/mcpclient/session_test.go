package mcpclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xolo-gateway/xolo/internal/adapter/mcpclient"
)

// sessionCounter counts the MCP sessions used on a test server.
//
// It reads the Mcp-Session-Id request header rather than touching the body: the
// initialize carries none and every later request of a session carries the one
// it opened, so the distinct IDs are the sessions opened. Reading the body would
// break the streaming handler.
type sessionCounter struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (c *sessionCounter) observe(r *http.Request) {
	id := r.Header.Get("Mcp-Session-Id")
	if id == "" {
		return
	}
	c.mu.Lock()
	c.ids[id] = struct{}{}
	c.mu.Unlock()
}

// sessions returns how many distinct sessions the server saw.
func (c *sessionCounter) sessions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

// newCountingServer starts an MCP server counting the sessions used on it.
func newCountingServer(t *testing.T) (*httptest.Server, *sessionCounter) {
	t.Helper()

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "v1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "greet", Description: "greets someone"}, greet)

	counter := &sessionCounter{ids: map[string]struct{}{}}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		counter.observe(r)
		handler.ServeHTTP(w, r)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, counter
}

// TestConnector_OneSessionAcrossToolRounds is the third acceptance criterion:
// a request resolving three tool calls opens a single MCP session.
func TestConnector_OneSessionAcrossToolRounds(t *testing.T) {
	srv, counter := newCountingServer(t)

	connector := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL + "/mcp"}, time.Minute)
	defer connector.Close()

	ctx := context.Background()
	for round := range 3 {
		err := connector.Do(ctx, func(session *mcp.ClientSession) error {
			tools, err := mcpclient.BuildTools(ctx, session, nil)
			if err != nil {
				return err
			}
			result, err := tools[0].Execute(ctx, map[string]any{"name": "xolo"})
			if err != nil {
				return err
			}
			if result.Text() != "hello xolo" {
				t.Errorf("round %d: expected %q, got %q", round, "hello xolo", result.Text())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("round %d: Do: %v", round, err)
		}
	}

	if got := counter.sessions(); got != 1 {
		t.Errorf("expected a single MCP session for three tool rounds, the server saw %d", got)
	}
}

// TestConnector_ConcurrentCallsShareOneSession makes sure two concurrent calls
// of one conversation do not race into two sessions.
func TestConnector_ConcurrentCallsShareOneSession(t *testing.T) {
	srv, counter := newCountingServer(t)

	connector := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL + "/mcp"}, time.Minute)
	defer connector.Close()

	ctx := context.Background()
	start := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			<-start
			errs <- connector.Do(ctx, func(session *mcp.ClientSession) error {
				_, _, err := mcpclient.CallToolText(ctx, session, "greet", `{"name":"xolo"}`)
				return err
			})
		}()
	}
	close(start)
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}

	if got := counter.sessions(); got != 1 {
		t.Errorf("expected concurrent calls of one conversation to share a session, the server saw %d", got)
	}
}

// TestConnector_RetriesOnceOnADeadSession checks a session that outlived its
// server is replaced rather than failing the call outright.
func TestConnector_RetriesOnceOnADeadSession(t *testing.T) {
	srv, _ := newCountingServer(t)

	connector := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL + "/mcp"}, time.Minute)
	defer connector.Close()

	ctx := context.Background()
	if err := connector.Do(ctx, func(session *mcp.ClientSession) error {
		_, _, err := mcpclient.CallToolText(ctx, session, "greet", `{"name":"a"}`)
		return err
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Drop the connection under the session: the next call has to notice the
	// session is over, and reconnect rather than reuse it.
	srv.CloseClientConnections()

	err := connector.Do(ctx, func(session *mcp.ClientSession) error {
		_, _, callErr := mcpclient.CallToolText(ctx, session, "greet", `{"name":"b"}`)
		return callErr
	})
	if err != nil {
		t.Fatalf("expected the call to be retried on a fresh session: %v", err)
	}
}

// newForgetfulServer starts an MCP server that can be told to forget one
// session, answering 404 to any request carrying it. That is how the
// specification says a server reports a session it terminated, and the SDK
// surfaces it as ErrSessionMissing, not ErrConnectionClosed.
//
// Only the named session is refused: a real server terminates specific
// sessions, so refusing every session id would also break the reconnect the
// retry performs.
func newForgetfulServer(t *testing.T) (srv *httptest.Server, forget func() bool, counter *sessionCounter) {
	t.Helper()

	counter = &sessionCounter{ids: map[string]struct{}{}}
	forgotten := &atomic.Pointer[string]{}
	lastSeen := &atomic.Pointer[string]{}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newGreetServer(t) }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		counter.observe(r)
		id := r.Header.Get("Mcp-Session-Id")
		if id == "" {
			handler.ServeHTTP(w, r)
			return
		}
		lastSeen.Store(&id)
		if want := forgotten.Load(); want != nil && *want == id {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		handler.ServeHTTP(w, r)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	forget = func() bool {
		seen := lastSeen.Load()
		if seen == nil {
			return false
		}
		forgotten.Store(seen)
		return true
	}
	return server, forget, counter
}

func newGreetServer(t *testing.T) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "greets someone"}, greet)
	return server
}

// TestConnector_RetriesOnceOnASessionMissing is the review's third defect: a
// server that terminated the session answers 404, reported as
// ErrSessionMissing, which is not ErrConnectionClosed. Without matching it, the
// call was treated as an ordinary failure: no retry, and the dead session kept
// serving the conversation.
func TestConnector_RetriesOnceOnASessionMissing(t *testing.T) {
	srv, forget, _ := newForgetfulServer(t)

	connector := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL + "/mcp"}, time.Minute)
	defer connector.Close()

	ctx := context.Background()
	if err := connector.Do(ctx, func(session *mcp.ClientSession) error {
		_, _, err := mcpclient.CallToolText(ctx, session, "greet", `{"name":"a"}`)
		return err
	}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// The server terminates the session the conversation has been using.
	if !forget() {
		t.Fatal("no session id was observed")
	}

	err := connector.Do(ctx, func(session *mcp.ClientSession) error {
		_, _, callErr := mcpclient.CallToolText(ctx, session, "greet", `{"name":"b"}`)
		return callErr
	})
	if err != nil {
		t.Fatalf("a session the server forgot must be retried, not surfaced: %v", err)
	}
}

// TestConnector_OnExpireOnlyOnRetirement pins down when a pool should drop its
// entry. Reset and SetConfig close the current session to open another one on
// the same connector, which the pool still serves: firing there evicted a live
// connector on the ordinary dead-session-retry and credential-rotation paths.
// Only idle expiry and Close retire it.
func TestConnector_OnExpireOnlyOnRetirement(t *testing.T) {
	srv, _ := newCountingServer(t)

	connector := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL + "/mcp"}, time.Minute)
	defer connector.Close()

	var fired atomic.Int64
	connector.OnExpire(func() { fired.Add(1) })

	ctx := context.Background()
	call := func() {
		t.Helper()
		if err := connector.Do(ctx, func(session *mcp.ClientSession) error {
			_, _, err := mcpclient.CallToolText(ctx, session, "greet", `{"name":"a"}`)
			return err
		}); err != nil {
			t.Fatalf("call: %v", err)
		}
	}

	call()
	if got := fired.Load(); got != 0 {
		t.Fatalf("OnExpire fired %d times while the session was live", got)
	}

	// Reset replaces the session, it does not retire the connector.
	connector.Reset()
	if got := fired.Load(); got != 0 {
		t.Errorf("Reset must not retire the connector, OnExpire fired %d times", got)
	}
	call()

	// Neither does a credential refresh.
	connector.SetConfig(mcpclient.Config{Endpoint: srv.URL + "/mcp", AuthValue: "rotated"})
	if got := fired.Load(); got != 0 {
		t.Errorf("SetConfig must not retire the connector, OnExpire fired %d times", got)
	}

	// Close does.
	connector.Close()
	waitFor(t, func() bool { return fired.Load() == 1 })
	if got := fired.Load(); got != 1 {
		t.Errorf("expected OnExpire to fire once on Close, fired %d times", got)
	}
}

// TestConnector_OnExpireOnIdleTimeout checks the other retirement path: a
// connector left unused is closed and releases its pool entry.
func TestConnector_OnExpireOnIdleTimeout(t *testing.T) {
	srv, _ := newCountingServer(t)

	// A short idle timeout keeps the test fast; expire ticks at a quarter of it.
	connector := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL + "/mcp"}, 200*time.Millisecond)
	defer connector.Close()

	var fired atomic.Int64
	connector.OnExpire(func() { fired.Add(1) })

	ctx := context.Background()
	if err := connector.Do(ctx, func(session *mcp.ClientSession) error {
		_, _, err := mcpclient.CallToolText(ctx, session, "greet", `{"name":"a"}`)
		return err
	}); err != nil {
		t.Fatalf("call: %v", err)
	}

	waitFor(t, func() bool { return fired.Load() == 1 })
	if got := fired.Load(); got != 1 {
		t.Errorf("expected OnExpire to fire once on idle expiry, fired %d times", got)
	}
}

// waitFor polls cond for a couple of seconds, failing the test if it never holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}

// TestConnector_SessionlessWhileConnecting checks the predicate a pool uses to
// evict: a connector whose handshake is in flight holds no session yet, but it
// is about to, and evicting it would leave the caller retrying on a connector
// the pool no longer holds.
func TestConnector_SessionlessWhileConnecting(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the handshake so the connector stays "connecting".
		<-release
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := mcpclient.NewConnector(mcpclient.Config{Endpoint: srv.URL}, time.Minute)
	defer c.Close()

	// A connector that never tried to connect is sessionless.
	if !c.Sessionless() {
		t.Error("a connector that never connected should be sessionless")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Do(t.Context(), func(*mcp.ClientSession) error { return nil })
	}()

	// While the handshake is blocked, it must not look evictable.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !c.Sessionless() {
			close(release)
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	<-done
	t.Error("a connector mid-handshake must not be reported sessionless")
}
