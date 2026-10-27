package mcpclient

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
)

// DefaultIdleTimeout is how long a Connector keeps its session open without
// being used, when it is left unset. It is deliberately generous: a session is
// what makes an MCP server's own per-conversation state survive the tool rounds
// of a request, and a model may think for a minute between two calls.
const DefaultIdleTimeout = 5 * time.Minute

// Connector opens MCP sessions with fixed connection settings and keeps its
// session alive between calls, so a request resolving several tool calls does
// not open a session per call.
//
// An MCP session is more than a connection: the Streamable HTTP transport pins
// the exchange to a session ID, and a server keeping per-session state (a
// database handle, a query plan, a paginated result set) loses it when a new one
// starts. A gateway opening a session per tool call therefore pays the
// initialization on every round and breaks any tool whose result refers to what
// a previous one left behind.
//
// Concurrent callers share the session; the MCP client session is itself
// concurrency safe. A session the server ended is detected by Do, which replaces
// it and retries once, rather than handing a dead session out.
type Connector struct {
	cfg         Config
	idleTimeout time.Duration
	now         func() time.Time

	mu       sync.Mutex
	session  *mcp.ClientSession
	inFlight int
	lastUsed time.Time
	closed   bool
	stop     chan struct{}
	// connectMu serializes the opens: two callers finding no session at the
	// same instant would otherwise each open one, defeating the very reuse the
	// connector exists for. It is held only while no session exists, so calls
	// on a live session never wait on it.
	connectMu sync.Mutex
	// generation increases on every credential change, so a handshake that
	// started before a SetConfig can tell its session carries the former ones.
	generation uint64
	// onExpire, when set, runs once the connector has been retired for good:
	// idle expiry, or Close. A pool needs it to drop the entry it keeps for
	// this connector, or every conversation leaves one behind for the life of
	// the process.
	//
	// It deliberately does not run on Reset or SetConfig: those close the
	// current session to open another one on the same connector, which the pool
	// still serves. Firing there would evict a live connector on the normal
	// dead-session-retry and credential-rotation paths.
	onExpire func()
}

// NewConnector returns a Connector opening sessions with cfg. idleTimeout <= 0
// falls back to DefaultIdleTimeout.
func NewConnector(cfg Config, idleTimeout time.Duration) *Connector {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &Connector{cfg: cfg, idleTimeout: idleTimeout, now: time.Now}
}

// SetConfig updates the settings the next session is opened with, and closes the
// current one when the credentials changed.
//
// A connector outlives the credentials it was created with: an OAuth access
// token expires, an administrator rotates a static secret. A session opened on
// the former keeps sending it on every exchange, since the transport pins the
// session ID and the headers with it, so a stale credential would fail every
// call of the conversation, retry included. Replacing the session is what makes
// the next call use the new ones.
func (c *Connector) SetConfig(cfg Config) {
	c.mu.Lock()
	changed := cfg.AuthValue != c.cfg.AuthValue ||
		cfg.AuthHeaderName != c.cfg.AuthHeaderName ||
		cfg.Endpoint != c.cfg.Endpoint ||
		cfg.Timeout != c.cfg.Timeout
	c.cfg = cfg
	if changed {
		c.generation++
		c.discardLocked()
	}
	c.mu.Unlock()
}

// errCredentialsChanged reports a handshake whose credentials were replaced
// before it completed. Callers may retry: the next attempt uses the new ones.
var errCredentialsChanged = errors.New("mcpclient: credentials changed while connecting")

// Sessionless reports whether the connector holds no session, which is the case
// for one whose connection attempt failed. A pool uses it to drop an entry that
// has nothing to expire and would otherwise stay forever.
func (c *Connector) Sessionless() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session == nil
}

// OnExpire registers a callback run when the connector is retired: on idle
// expiry or on Close. It runs at most once, from a goroutine of its own, and must
// not block.
//
// It deliberately does not run on Reset or SetConfig. Those close the current
// session to open another one on the same connector, which a pool still serves;
// firing there would evict a live entry on the ordinary dead-session-retry and
// credential-rotation paths.
func (c *Connector) OnExpire(f func()) {
	c.mu.Lock()
	c.onExpire = f
	c.mu.Unlock()
}

// Do runs fn on the connector's session, opening one when there is none.
//
// A call failing because the session is over (the server restarted, a proxy
// dropped the connection) is retried once on a fresh session: a session kept
// between requests is exactly the thing that outlives the server it was opened
// on, and one retry is what tells a stale session from an operation that fails
// on its own.
func (c *Connector) Do(ctx context.Context, fn func(*mcp.ClientSession) error) error {
	session, err := c.acquire(ctx)
	if errors.Is(err, errCredentialsChanged) {
		// The handshake raced a credential refresh: retrying is the whole point
		// of reporting it rather than handing out a session on stale ones.
		session, err = c.acquire(ctx)
	}
	if err != nil {
		return err
	}

	err = fn(session)
	if err == nil || !IsSessionOver(err) {
		c.done()
		return err
	}

	// The session is over: drop it, unless a concurrent caller already replaced
	// it (ours is then stale and theirs must be left alone), and try once on a
	// new one.
	c.done()
	c.discardIf(session)

	retry, retryErr := c.acquire(ctx)
	if retryErr != nil {
		return retryErr
	}
	defer c.done()
	return fn(retry)
}

// IsSessionOver reports whether err is the session having ended rather than the
// operation having failed.
//
// It is exported for callers that run inside Connector.Do and must let such an
// error escape so the one retry happens, instead of degrading it to text.
//
// Two shapes say the session is over. A dropped transport closes the connection
// (ErrConnectionClosed). A Streamable HTTP server that terminated the session
// answers 404, which the SDK reports as ErrSessionMissing. Matching only the
// first would treat the second as an ordinary failure: no retry, no Reset, and
// the dead session would keep serving every later call of the conversation
// until the idle timeout, which an active conversation never reaches.
func IsSessionOver(err error) bool {
	return errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, mcp.ErrSessionMissing)
}

// acquire returns a session for one call, opening it when needed. Every
// successful acquire must be matched by a done.
func (c *Connector) acquire(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	if c.session != nil && !usable(c.session) {
		c.discardLocked()
	}
	if c.session != nil {
		c.inFlight++
		c.lastUsed = c.now()
		session := c.session
		c.mu.Unlock()
		return session, nil
	}
	c.mu.Unlock()

	// Only one caller opens a session at a time: the others wait here and then
	// find the one it opened. Holding c.mu itself through the handshake would
	// serialize every call, including those on a live session.
	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	// Someone may have opened a session while we waited.
	c.mu.Lock()
	if c.session != nil {
		c.inFlight++
		c.lastUsed = c.now()
		session := c.session
		c.mu.Unlock()
		return session, nil
	}
	closed := c.closed
	// Read under the lock: SetConfig writes them on every request, so reading
	// them here unlocked would race with the very refresh the connector exists
	// for.
	cfg := c.cfg
	generation := c.generation
	c.mu.Unlock()
	if closed {
		return nil, errors.New("mcpclient: connector closed")
	}

	session, err := Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = session.Close()
		return nil, errors.New("mcpclient: connector closed")
	}
	if c.generation != generation {
		// The credentials were replaced while this session was being opened:
		// installing it would send the former ones, which is exactly what
		// SetConfig exists to prevent. Drop it and let the caller retry on the
		// new credentials rather than hand out a session pinned to the old.
		c.mu.Unlock()
		_ = session.Close()
		return nil, errCredentialsChanged
	}
	if c.session != nil {
		// Unreachable while connectMu is held: kept so a future change to the
		// locking cannot silently leak the session just opened.
		c.inFlight++
		c.lastUsed = c.now()
		existing := c.session
		c.mu.Unlock()
		_ = session.Close()
		return existing, nil
	}
	c.session = session
	c.inFlight = 1
	c.lastUsed = c.now()
	c.stop = make(chan struct{})
	stop := c.stop
	c.mu.Unlock()

	go c.expire(stop)

	return session, nil
}

// done ends one call on the session. It is what starts the idle countdown.
func (c *Connector) done() {
	c.mu.Lock()
	if c.inFlight > 0 {
		c.inFlight--
	}
	c.lastUsed = c.now()
	c.mu.Unlock()
}

// discardIf closes the current session only when it is still the one given,
// and reports whether it did.
//
// A retry that failed on a session must not discard the replacement another
// caller installed meanwhile: doing so would close the session that caller is
// already retrying on, and its own retry would then fail.
func (c *Connector) discardIf(failed *mcp.ClientSession) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil || c.session != failed {
		return false
	}
	c.discardLocked()
	return true
}

// Reset closes the current session so the next call opens a new one: a plugin
// calls it when the credentials behind the settings changed.
func (c *Connector) Reset() {
	c.mu.Lock()
	c.discardLocked()
	c.mu.Unlock()
}

// Close closes the current session and marks the Connector terminal: a later Do
// returns an error rather than opening a new session. It is what a plugin calls
// when it shuts down, not a way to recycle a connector.
func (c *Connector) Close() {
	c.mu.Lock()
	c.closed = true
	session := c.session
	c.retireLocked()
	c.mu.Unlock()

	// The retirement path closes the session from a goroutine, which is right on
	// the request path (a caller gone must not wait) and wrong on shutdown: a
	// process exiting right after Close would leave the server holding the
	// stream this method exists to release. Closing again is harmless, the
	// session's Close is idempotent.
	if session != nil {
		_ = session.Close()
	}
}

// usable reports whether session can serve a call now.
//
// Liveness is not probed here: the MCP client session offers no non-blocking
// check (Wait blocks until the session ends). A dead session is caught by Do,
// which retries once, and a session that never finished its handshake is caught
// by its missing initialize result.
func usable(session *mcp.ClientSession) bool {
	return session.InitializeResult() != nil
}

// discardLocked closes the current session, if any, keeping the connector
// usable: the next call opens another one. The caller holds c.mu.
func (c *Connector) discardLocked() {
	session, stop := c.session, c.stop
	c.session, c.stop, c.inFlight = nil, nil, 0
	if session == nil {
		return
	}
	if stop != nil {
		close(stop)
	}
	// Closing waits for nothing: a caller gone during its tool round must not
	// keep the session open. The MCP session fails its in-flight calls with
	// ErrConnectionClosed, which Do turns into one retry.
	go func() { _ = session.Close() }()
}

// retireLocked closes the current session and notifies the pool that this
// connector is gone for good. The caller holds c.mu.
func (c *Connector) retireLocked() {
	onExpire := c.onExpire
	c.onExpire = nil
	c.discardLocked()
	if onExpire != nil {
		// Deferred: the callback takes the pool's lock, which must not be
		// taken while c.mu is held (the pool calls SetConfig, which takes c.mu).
		go onExpire()
	}
}

// expire closes the session once it has been idle for idleTimeout.
func (c *Connector) expire(stop chan struct{}) {
	ticker := time.NewTicker(c.idleTimeout / 4)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.stop != stop {
				// Replaced meanwhile: this goroutine is the stale one.
				c.mu.Unlock()
				return
			}
			if c.inFlight == 0 && c.now().Sub(c.lastUsed) >= c.idleTimeout {
				c.retireLocked()
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
		}
	}
}
