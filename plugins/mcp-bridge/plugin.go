package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
	"github.com/xolo-gateway/xolo/internal/adapter/mcpclient"
	"github.com/xolo-gateway/xolo/pkg/pluginsdk"
	proto "github.com/xolo-gateway/xolo/pkg/pluginsdk/proto"
)

const secretKeyAuthValue = "authValue"

// idleTimeout is how long an MCP session of a conversation stays open without
// being used. A conversation outlives a request — an agent thinking for a
// minute between two tool calls, then asking again — so the session is kept,
// but not forever: a server that is not called for this long is worth
// reconnecting to when it is next asked for.
const idleTimeout = mcpclient.DefaultIdleTimeout

// Plugin implémente proto.XoloPluginServer.
type Plugin struct {
	proto.UnimplementedXoloPluginServer

	mu         sync.Mutex
	hostClient pluginsdk.HostClient
	oauth      *oauthClient
	// sessions holds one MCP session per server of a conversation, so the tool
	// rounds of a request share it instead of opening a session each time.
	sessions map[string]*mcpclient.Connector
	// keys is the reverse index, so a connector's expiry can clear its own
	// entry without any caller having to rebuild the key.
	keys map[*mcpclient.Connector]string
}

// oauthClient returns the client shared by the tool calls and the UI.
func (p *Plugin) oauthClient() *oauthClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oauth == nil {
		p.oauth = newOAuthClient()
	}
	return p.oauth
}

// SetHostClient implémente pluginsdk.HostClientSetter.
func (p *Plugin) SetHostClient(c pluginsdk.HostClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hostClient = c
}

func (p *Plugin) getHostClient() pluginsdk.HostClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hostClient
}

// connector returns the session holder of one conversation on one endpoint,
// creating it on first use.
//
// The key mixes the caller identity, the conversation, the node and the
// endpoint. The identity is part of it even though the conversation usually
// encodes it: the session id comes from the client, so two callers sending the
// same value would otherwise be handed one MCP session, and with it the first
// caller's credentials and whatever the server holds for that session.
//
// The credentials are pushed into the connector on every call, not only when
// it is created: an OAuth token expires and a static secret gets rotated, and a
// pooled session keeps sending whatever it was opened with.
func (p *Plugin) connector(ec sessionScope, cfg Config) *mcpclient.Connector {
	key := poolKey(ec, cfg.Endpoint)

	connCfg := mcpclient.Config{
		Endpoint:       cfg.Endpoint,
		AuthHeaderName: ec.authHeaderName,
		AuthValue:      ec.authValue,
		Timeout:        time.Duration(cfg.TimeoutSeconds) * time.Second,
	}

	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = map[string]*mcpclient.Connector{}
		p.keys = map[*mcpclient.Connector]string{}
	}
	existing, found := p.sessions[key]
	p.mu.Unlock()

	if found {
		// A connector can be retired between this lookup and now (its idle timer
		// fired, or Close ran), and retirement is terminal: handing it out would
		// fail the request with ErrConnectorClosed at the idle boundary. The
		// check cannot be atomic with the hand-out, because SetConfig has to run
		// outside p.mu (it can close a session, whose expiry callback takes
		// p.mu). So the decision is taken after SetConfig, and a connector the
		// ticker retired in between is treated as a pool miss.
		existing.SetConfig(connCfg)
		if !existing.Terminal() {
			return existing
		}
		p.forgetKey(existing)
	}

	c := mcpclient.NewConnector(connCfg, idleTimeout)
	// Registered before the connector is published, and on an object nobody
	// else holds yet: a callback taking p.mu must never be installed while
	// p.mu is held.
	c.OnExpire(func() { p.forgetKey(c) })

	p.mu.Lock()
	// Close nils both maps. Re-create them rather than write into a nil map:
	// a shutdown landing between the two critical sections would otherwise
	// panic here and abort the plugin process instead of failing one request.
	if p.sessions == nil {
		p.sessions = map[string]*mcpclient.Connector{}
		p.keys = map[*mcpclient.Connector]string{}
	}
	// Another caller may have created one while this one was being built.
	if current, ok := p.sessions[key]; ok {
		p.mu.Unlock()
		// Ours was never published: no entry to clear, and closing it is what
		// stops it from lingering with a live expiry callback.
		c.Close()
		return current
	}
	// Both maps are written in this one critical section: p.keys is read and
	// deleted under p.mu elsewhere, and concurrent map writes abort the whole
	// plugin process rather than the request.
	p.sessions[key] = c
	p.keys[c] = key
	p.mu.Unlock()
	return c
}

// dropIfSessionless removes a pooled connector that never managed to open a
// session.
//
// A connector only retires itself once it has a session to close, so one whose
// first Connect failed would sit in the pool for the life of the process:
// nothing to expire, and no other path deleting it. Called when a call failed to
// obtain a session at all.
//
// The liveness check and the eviction happen under p.mu in one step: checking
// here and evicting in a second call would let a concurrent request open a
// session on the very connector being dropped, leaving it unreachable from the
// pool and diverging the conversation onto a second session.
func (p *Plugin) dropIfSessionless(c *mcpclient.Connector) {
	p.mu.Lock()
	key, ok := p.keys[c]
	if !ok {
		p.mu.Unlock()
		return
	}
	if current, still := p.sessions[key]; still && current == c && !c.Sessionless() {
		// Another request reached it and opened a session (or is opening one):
		// it is live.
		p.mu.Unlock()
		return
	}
	delete(p.sessions, key)
	delete(p.keys, c)
	p.mu.Unlock()

	// Closed outside the pool lock: Close is a network exchange, and holding
	// p.mu through it would block every request of every conversation behind
	// one unresponsive MCP server.
	c.Close()
}

// poolKey names the pool entry of one conversation on one endpoint. The caller
// identity is part of it even though the conversation usually encodes it: the
// session id comes from the client, so two callers sending the same value would
// otherwise be handed one MCP session, and with it the first caller's
// credentials and whatever the server holds for that session.
func poolKey(ec sessionScope, endpoint string) string {
	return strings.Join([]string{ec.orgID, ec.userID, ec.key, ec.nodeID, endpoint}, "\x00")
}

// forgetKey removes a connector from the pool, whichever entry holds it.
func (p *Plugin) forgetKey(c *mcpclient.Connector) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key, ok := p.keys[c]
	if !ok {
		return
	}
	if current, still := p.sessions[key]; still && current == c {
		delete(p.sessions, key)
	}
	delete(p.keys, c)
}

// Close closes every pooled MCP session.
//
// Sessions are kept between requests on purpose, so nothing on the request path
// closes them: a plugin process shutting down has to, or the servers it talked to
// are left holding a stream open.
func (p *Plugin) Close() {
	p.mu.Lock()
	connectors := make([]*mcpclient.Connector, 0, len(p.sessions))
	for _, c := range p.sessions {
		connectors = append(connectors, c)
	}
	p.sessions = nil
	p.keys = nil
	p.mu.Unlock()

	for _, c := range connectors {
		c.Close()
	}
}

// sessionScope is everything the key of a conversation session is made of,
// resolved from the request.
type sessionScope struct {
	orgID          string
	userID         string
	key            string
	nodeID         string
	authHeaderName string
	authValue      string
}

// connect resolves the node's configuration and credentials, and returns the
// connector holding the conversation's session. The caller runs its work
// through its Do, which opens the session and replaces it when it died.
//
// pooled reports whether the connector belongs to the shared pool. When it does
// not (no conversation id, so nothing to key it on), the caller owns it and has
// to close it once done.
func (p *Plugin) connect(ctx context.Context, reqCtx *proto.RequestContext) (cfgOut Config, connector *mcpclient.Connector, pooled bool, err error) {
	cfg, err := parseConfig(reqCtx.GetConfigJson())
	if err != nil {
		return Config{}, nil, false, errors.Wrap(err, "parse config")
	}
	if cfg.Endpoint == "" {
		return Config{}, nil, false, errors.New("mcp-bridge: endpoint not configured")
	}

	authHeaderName := cfg.AuthHeaderName
	var authValue string
	if cfg.AuthMode == AuthModeOAuth {
		hc := p.getHostClient()
		if hc == nil {
			return cfg, nil, false, errors.WithStack(ErrNotAuthorized)
		}
		token, err := p.oauthClient().accessToken(ctx, hc, "mcp-bridge", reqCtx.GetNodeId(), reqCtx.GetUserId(), cfg.Endpoint)
		if err != nil {
			return cfg, nil, false, err
		}
		authHeaderName, authValue = "Authorization", "Bearer "+token
	} else if hc := p.getHostClient(); hc != nil {
		v, found, err := hc.GetSecret(ctx, reqCtx.GetSecretScopeId(), "mcp-bridge", reqCtx.GetNodeId(), secretKeyAuthValue)
		if err != nil {
			return Config{}, nil, false, errors.Wrap(err, "get auth secret")
		}
		if found {
			authValue = v
		}
	}

	connCfg := mcpclient.Config{
		Endpoint:       cfg.Endpoint,
		AuthHeaderName: authHeaderName,
		AuthValue:      authValue,
		Timeout:        time.Duration(cfg.TimeoutSeconds) * time.Second,
	}

	// A request with no conversation id has nothing to key a session on: it gets
	// one of its own, closed once the call is done, rather than sharing the
	// caller's state across unrelated conversations.
	if sessionKey(reqCtx) == "" {
		return cfg, mcpclient.NewConnector(connCfg, idleTimeout), false, nil
	}

	return cfg, p.connector(sessionScope{
		orgID:          reqCtx.GetOrgId(),
		userID:         reqCtx.GetUserId(),
		key:            sessionKey(reqCtx),
		nodeID:         reqCtx.GetNodeId(),
		authHeaderName: authHeaderName,
		authValue:      authValue,
	}, cfg), true, nil
}

// sessionKey names the conversation a request belongs to, or empty when the
// client sent no conversation id.
//
// An empty key is not a reason to invent one. Falling back to the caller
// identity would make every conversation of a user share one MCP session: two
// tabs, or an agent and an interactive prompt, would read and write the same
// server-side state, which is a regression against the one-session-per-call it
// replaced. A request with no conversation is served by a session of its own.
func sessionKey(reqCtx *proto.RequestContext) string {
	return reqCtx.GetSessionId()
}

func (p *Plugin) Describe(_ context.Context, _ *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:         "mcp-bridge",
		Version:      "0.2.0",
		Description:  "Connecte un serveur MCP (Streamable HTTP) externe : expose ses tools au LLM, résout les appels d'outils côté serveur et injecte les tools et ressources configurés avant le premier appel au modèle.",
		Capabilities: []proto.PluginDescriptor_Capability{proto.PluginDescriptor_TOOL_PROVIDER},
		ConfigSchema: configSchemaJSON,
		InputPorts: []*proto.PortDescriptor{
			{Name: "request", PortType: "request", Required: true},
		},
		OutputPorts: []*proto.PortDescriptor{
			{Name: "request", PortType: "request", Required: true},
		},
	}, nil
}

func (p *Plugin) ListTools(ctx context.Context, in *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
	cfg, connector, pooled, err := p.connect(ctx, in.GetCtx())
	if errors.Is(err, ErrNotAuthorized) {
		// The user has to authorize the server first: the only tool offered
		// gives the link to do so.
		return &proto.ListToolsOutput{
			Tools: []*proto.ToolDescriptor{{
				Name:            connectToolName,
				Description:     "Donne le lien permettant à l'utilisateur d'autoriser l'accès au serveur MCP " + cfg.Endpoint + ". À appeler lorsque l'utilisateur demande une information que ce serveur pourrait fournir.",
				InputSchemaJson: `{"type":"object","properties":{}}`,
			}},
			MaxConsecutiveToolCalls: int32(cfg.MaxConsecutiveToolCalls),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if !pooled {
		// Nothing else keys a session for this request: it owns the connector.
		defer connector.Close()
	}

	var tools []*proto.ToolDescriptor
	var values []*proto.ConversationValue
	err = connector.Do(ctx, func(session *mcp.ClientSession) error {
		infos, err := mcpclient.ListToolInfos(ctx, session, cfg.ToolFilter)
		if err != nil {
			return err
		}
		tools = make([]*proto.ToolDescriptor, 0, len(infos))
		for _, info := range infos {
			tools = append(tools, &proto.ToolDescriptor{
				Name:            info.Name,
				Description:     info.Description,
				InputSchemaJson: info.InputSchemaJSON,
			})
		}
		var readErr error
		values, readErr = readResources(ctx, session, cfg.Resources)
		return readErr
	})
	if err != nil {
		p.dropIfSessionless(connector)
		return nil, err
	}

	return &proto.ListToolsOutput{
		Tools:                   tools,
		MaxConsecutiveToolCalls: int32(cfg.MaxConsecutiveToolCalls),
		ConversationTools:       conversationTools(cfg, tools),
		ConversationValues:      values,
	}, nil
}

// readResources reads the configured resources on the session ListTools opened.
//
// A resource that is absent, renamed by the server, or on a server without the
// resources capability becomes text in the injected block: a configuration
// mistake should let the conversation through, with a note the operator sees in
// the model's answer, and asking a server that advertised nothing would only
// produce an error.
//
// A session that died is returned as an error instead, never turned into text:
// the caller runs inside Connector.Do, whose one retry only happens when the
// session error escapes.
func readResources(ctx context.Context, session *mcp.ClientSession, refs []string) ([]*proto.ConversationValue, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	values := make([]*proto.ConversationValue, 0, len(refs))
	for _, ref := range refs {
		info, err := mcpclient.FindResource(ctx, session, ref)
		if err != nil {
			if mcpclient.IsSessionOver(err) {
				return nil, err
			}
			values = append(values, &proto.ConversationValue{
				Label:   "MCP resource " + ref,
				Content: "the resource could not be read: " + err.Error(),
			})
			continue
		}
		text, err := mcpclient.ReadResourceText(ctx, session, info.URI)
		if err != nil {
			if mcpclient.IsSessionOver(err) {
				return nil, err
			}
			values = append(values, &proto.ConversationValue{
				Label:   "MCP resource " + ref,
				Content: "the resource could not be read: " + err.Error(),
			})
			continue
		}
		label := "MCP resource " + info.URI
		if info.Name != "" && info.Name != ref {
			label = "MCP resource " + info.Name + " (" + info.URI + ")"
		}
		values = append(values, &proto.ConversationValue{Label: label, Content: text})
	}
	return values, nil
}

// conversationTools declares the tool the node asks the host to resolve before
// the model's first call, when the configuration names one. The declaration is
// dropped when the tool is not in the allowed list: the filter is what an
// administrator authorized, and a prefetch is no way around it.
func conversationTools(cfg Config, tools []*proto.ToolDescriptor) []*proto.ToolConversation {
	name := strings.TrimSpace(cfg.PrefetchTool)
	if name == "" {
		return nil
	}
	var declared *proto.ToolDescriptor
	for _, td := range tools {
		if td.GetName() == name {
			declared = td
			break
		}
	}
	if declared == nil {
		return nil
	}

	arguments := strings.TrimSpace(cfg.PrefetchArguments)
	if arguments == "" {
		arguments = "{}"
	}

	return []*proto.ToolConversation{{
		Trigger:        proto.ToolConversation_ALWAYS,
		Tool:           declared,
		ArgumentsJson:  arguments,
		ResultTemplate: cfg.PrefetchResultTemplate,
		Label:          cfg.PrefetchLabel,
	}}
}

func (p *Plugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	cfg, connector, pooled, err := p.connect(ctx, in.GetCtx())
	if errors.Is(err, ErrNotAuthorized) {
		return &proto.CallToolOutput{
			ResultText: "L'utilisateur doit d'abord autoriser l'accès au serveur MCP en ouvrant ce lien, puis renouveler sa demande : " + connectURL(cfg, in.GetCtx().GetNodeId()),
			IsError:    in.Name != connectToolName,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if !pooled {
		// A per-request connector: closed on return, or it outlives the request
		// that created it with nobody holding a reference to it.
		defer connector.Close()
	}

	var text string
	var isError bool
	err = connector.Do(ctx, func(session *mcp.ClientSession) error {
		var callErr error
		text, isError, callErr = mcpclient.CallToolText(ctx, session, in.Name, in.ArgumentsJson)
		return callErr
	})
	if err != nil {
		p.dropIfSessionless(connector)
		return nil, err
	}
	return &proto.CallToolOutput{ResultText: text, IsError: isError}, nil
}
