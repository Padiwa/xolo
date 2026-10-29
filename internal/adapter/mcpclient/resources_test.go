package mcpclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xolo-gateway/xolo/internal/adapter/mcpclient"
)

// newResourceServer starts an MCP server exposing a text resource and a binary
// one, plus a tool, so a test can tell what ListResources saw from what
// ReadResource returned.
func newResourceServer(t *testing.T) *httptest.Server {
	t.Helper()

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "v1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "greet", Description: "greets someone"}, greet)

	mcpServer.AddResource(
		&mcp.Resource{
			URI:         "file:///contexte/procedure.md",
			Name:        "procedure",
			Description: "la procédure interne",
			MIMEType:    "text/markdown",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI:      "file:///contexte/procedure.md",
				MIMEType: "text/markdown",
				Text:     "# Procédure\n\nOuvrir un ticket.",
			}}}, nil
		},
	)

	mcpServer.AddResource(
		&mcp.Resource{URI: "file:///logo.png", Name: "logo", MIMEType: "image/png"},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI:      "file:///logo.png",
				MIMEType: "image/png",
				Blob:     []byte{0x89, 0x50, 0x4e, 0x47},
			}}}, nil
		},
	)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestListResourceInfos(t *testing.T) {
	srv := newResourceServer(t)

	session, err := mcpclient.Connect(context.Background(), mcpclient.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	infos, err := mcpclient.ListResourceInfos(context.Background(), session)
	if err != nil {
		t.Fatalf("ListResourceInfos: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 resources, got %d (%v)", len(infos), infos)
	}
	if infos[0].URI != "file:///contexte/procedure.md" || infos[0].Name != "procedure" {
		t.Errorf("unexpected first resource: %+v", infos[0])
	}
}

// TestFindResource covers the two ways a configuration may name a resource: by
// URI, and by the name the server gives it.
func TestFindResource(t *testing.T) {
	srv := newResourceServer(t)

	session, err := mcpclient.Connect(context.Background(), mcpclient.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	ctx := context.Background()
	for _, ref := range []string{"file:///contexte/procedure.md", "procedure"} {
		info, err := mcpclient.FindResource(ctx, session, ref)
		if err != nil {
			t.Fatalf("FindResource(%q): %v", ref, err)
		}
		if info.URI != "file:///contexte/procedure.md" {
			t.Errorf("FindResource(%q) = %q, want the procedure URI", ref, info.URI)
		}
	}

	if _, err := mcpclient.FindResource(ctx, session, "does-not-exist"); err == nil {
		t.Error("expected an unknown resource to be reported")
	}
}

// TestReadResourceText checks a text resource is returned whole, and a binary
// one is reported rather than embedded.
func TestReadResourceText(t *testing.T) {
	srv := newResourceServer(t)

	session, err := mcpclient.Connect(context.Background(), mcpclient.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	ctx := context.Background()
	text, err := mcpclient.ReadResourceText(ctx, session, "file:///contexte/procedure.md")
	if err != nil {
		t.Fatalf("ReadResourceText: %v", err)
	}
	if text != "# Procédure\n\nOuvrir un ticket." {
		t.Errorf("unexpected resource text: %q", text)
	}

	binary, err := mcpclient.ReadResourceText(ctx, session, "file:///logo.png")
	if err != nil {
		t.Fatalf("ReadResourceText(binary): %v", err)
	}
	if binary != "[image/png content of file:///logo.png, not shown]" {
		t.Errorf("unexpected binary rendering: %q", binary)
	}
}

// TestListResourceInfos_WithoutCapability makes sure a server with no resources
// is asked nothing, rather than failed on.
func TestListResourceInfos_WithoutCapability(t *testing.T) {
	srv := newTestServer(t, "")

	session, err := mcpclient.Connect(context.Background(), mcpclient.Config{Endpoint: srv.URL + "/mcp"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	infos, err := mcpclient.ListResourceInfos(context.Background(), session)
	if err != nil {
		t.Fatalf("a server without resources must not fail: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("expected no resources, got %v", infos)
	}
}

// TestListResourceInfos_StopsOnCursorCycle pins the guard against a server that
// cycles its pagination instead of ending it. Comparing with the previous cursor
// alone lets an A -> B -> A alternation loop forever, and request contexts often
// carry no overall deadline.
//
// The fake answers initialize (so the resources capability is advertised) and
// then hands out an endless two-cursor cycle.
func TestListResourceInfos_StopsOnCursorCycle(t *testing.T) {
	var mu sync.Mutex
	pages := 0
	cursors := []string{"A", "B"}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		id := string(req.ID)
		if id == "" {
			id = "1"
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":"2025-06-18","capabilities":{"resources":{}},"serverInfo":{"name":"cycle","version":"v1"}}}`))
		case "resources/list":
			mu.Lock()
			next := cursors[pages%len(cursors)]
			pages++
			n := pages
			mu.Unlock()
			_, _ = fmt.Fprintf(w,
				`{"jsonrpc":"2.0","id":%s,"result":{"resources":[{"uri":"file:///r%d","name":"r%d"}],"nextCursor":%q}}`,
				id, n, n, next)
		default:
			// notifications/initialized and anything else: an empty result.
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}`))
		}
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	done := make(chan error, 1)
	go func() {
		session, err := mcpclient.Connect(context.Background(), mcpclient.Config{Endpoint: srv.URL})
		if err != nil {
			done <- err
			return
		}
		defer session.Close()
		_, err = mcpclient.ListResourceInfos(context.Background(), session)
		done <- err
	}()

	select {
	case err := <-done:
		mu.Lock()
		served := pages
		mu.Unlock()
		if err == nil {
			t.Errorf("expected a cycling cursor to be reported after %d pages", served)
		}
		if served < 2 {
			t.Errorf("the loop never paged (served %d), the test proves nothing", served)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListResourceInfos looped on a cycling cursor")
	}
}
