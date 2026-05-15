// Package ws implements suggest.Provider on top of Perplexity's
// WebSocket suggestion endpoint at wss://suggest.perplexity.ai/suggest/ws.
//
// Discovered wire protocol
// ------------------------
// One persistent, multiplexed WebSocket connection serves many requests,
// correlated by UUID.
//
// Client -> Server (JSON object):
//	{"q": "<query>", "uuid": "<uuid-v4>", "full_completion": true}
//
// Server -> Client (JSON positional array):
//	["<echoed query>", ["sugg1", "sugg2", ...], "<uuid-v4>", ...]
//
//	index 0 — echoed query
//	index 1 — suggestions array
//	index 2 — UUID that correlates the response back to a request
//
// Architecture
// ------------
// One reader goroutine drains the socket and dispatches each message
// to the waiting caller by UUID. Writes are serialized by a mutex so
// concurrent suggest calls don't interleave frames. A separate
// reconnect goroutine fires when the reader exits.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/yourname/perplexity-suggest/internal/suggest"
)

// Config is the subset of service config the client needs. Keeping this
// narrow makes the client easy to test without faking the full Config.
type Config struct {
	URL                  string
	Origin               string
	UserAgent            string
	DialTimeout          time.Duration
	RequestTimeout       time.Duration
	MaxRetries           int
	ReconnectDelay       time.Duration
	MaxReconnectAttempts int
}

// Client is a multiplexing WebSocket client for the Perplexity suggest
// endpoint. The zero value is not usable — construct with New.
type Client struct {
	cfg Config
	log *slog.Logger

	// connMu guards conn. It is *not* used to serialize writes — see writeMu.
	connMu sync.RWMutex
	conn   *websocket.Conn

	// writeMu serializes WebSocket writes. Per gorilla/websocket's docs:
	// only one goroutine may write at a time.
	writeMu sync.Mutex

	// pendingMu guards pending. We use RWMutex because dispatch (read)
	// is the hot path under load.
	pendingMu sync.RWMutex
	pending   map[string]chan suggestResult

	// connected reflects whether we have a live socket. Set/cleared by
	// the lifecycle code (connect, readLoop exit) and read by Suggest.
	connected atomic.Bool

	// reconnectInFlight prevents two concurrent reconnect loops.
	reconnectInFlight atomic.Bool

	// shutdownCtx is canceled on Close to stop background goroutines.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

type suggestResult struct {
	suggestions []string
	err         error
}

// New constructs a Client. It does NOT dial — call Connect to do so.
func New(cfg Config, log *slog.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		cfg:            cfg,
		log:            log,
		pending:        make(map[string]chan suggestResult),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
}

// Source identifies this provider in API responses.
func (c *Client) Source() string { return "api" }

// Connect establishes the WebSocket. Safe to call multiple times — a
// no-op if already connected.
func (c *Client) Connect(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn != nil {
		return nil
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: c.cfg.DialTimeout,
	}
	headers := http.Header{}
	headers.Set("Origin", c.cfg.Origin)
	headers.Set("User-Agent", c.cfg.UserAgent)
	headers.Set("Accept-Language", "en-US,en;q=0.9")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")

	c.log.Info("dialing WebSocket", "url", c.cfg.URL)
	conn, _, err := dialer.DialContext(ctx, c.cfg.URL, headers)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}

	c.conn = conn
	c.connected.Store(true)
	go c.readLoop(conn)
	c.log.Info("WebSocket connected")
	return nil
}

// Close terminates the connection and stops all background work.
func (c *Client) Close() error {
	c.shutdownCancel()
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil {
		// Polite close, then hard close.
		_ = c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected.Store(false)
	c.failAllPending(suggest.ErrUnavailable)
	return nil
}

// Suggest implements suggest.Provider. It registers a UUID, sends one
// frame, and waits for the matching response. On transient errors it
// retries up to cfg.MaxRetries times with jittered exponential backoff.
func (c *Client) Suggest(ctx context.Context, query string) ([]string, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt, 500*time.Millisecond)
			c.log.Warn("retrying suggest", "attempt", attempt, "wait", wait, "err", lastErr)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %w", suggest.ErrTimeout, ctx.Err())
			}
		}

		suggestions, err := c.suggestOnce(ctx, query)
		if err == nil {
			return suggestions, nil
		}
		lastErr = err

		// Only retry on transient classes of error.
		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) suggestOnce(ctx context.Context, query string) ([]string, error) {
	if !c.connected.Load() {
		// Kick a reconnect attempt in the background; this request fails
		// fast and the retry loop will get another shot.
		c.triggerReconnect()
		return nil, fmt.Errorf("%w: WebSocket not connected", suggest.ErrUnavailable)
	}

	reqID := uuid.NewString()
	ch := make(chan suggestResult, 1)

	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	payload, _ := json.Marshal(struct {
		Q              string `json:"q"`
		UUID           string `json:"uuid"`
		FullCompletion bool   `json:"full_completion"`
	}{Q: query, UUID: reqID, FullCompletion: true})

	if err := c.writeMessage(payload); err != nil {
		return nil, fmt.Errorf("%w: write: %w", suggest.ErrUpstream, err)
	}

	// Apply per-request timeout, but only if the caller didn't already
	// set a shorter deadline.
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.suggestions, nil
	case <-reqCtx.Done():
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: no response within %s", suggest.ErrTimeout, c.cfg.RequestTimeout)
		}
		return nil, fmt.Errorf("%w: %w", suggest.ErrTimeout, reqCtx.Err())
	}
}

func (c *Client) writeMessage(payload []byte) error {
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn == nil {
		return errors.New("nil connection")
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// readLoop drains the socket. It runs until ReadMessage returns an
// error, at which point it marks the client disconnected, fails every
// pending request, and triggers reconnect.
func (c *Client) readLoop(conn *websocket.Conn) {
	defer func() {
		c.connected.Store(false)
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		c.failAllPending(suggest.ErrUnavailable)
		// Only schedule a reconnect if we weren't shut down on purpose.
		if c.shutdownCtx.Err() == nil {
			c.triggerReconnect()
		}
	}()

	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.log.Warn("WebSocket read error", "err", err)
			} else {
				c.log.Info("WebSocket closed", "err", err)
			}
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}
		c.dispatch(raw)
	}
}

// dispatch parses one incoming frame and routes it to the waiting
// caller. The wire format is a positional JSON array — see the package
// doc comment.
func (c *Client) dispatch(raw []byte) {
	var top []json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		c.log.Warn("dropping non-array message", "err", err)
		return
	}
	if len(top) < 3 {
		c.log.Warn("dropping short message", "len", len(top))
		return
	}

	var reqID string
	if err := json.Unmarshal(top[2], &reqID); err != nil {
		c.log.Warn("dropping message: bad uuid", "err", err)
		return
	}

	// Tolerate non-array index 1 by returning an empty slice — mirrors
	// the Python implementation. The spec says "empty array when no
	// suggestions", and that's what we send back.
	suggestions := []string{}
	_ = json.Unmarshal(top[1], &suggestions)

	c.pendingMu.RLock()
	ch, ok := c.pending[reqID]
	c.pendingMu.RUnlock()
	if !ok {
		c.log.Debug("response for unknown uuid (likely already timed out)", "uuid", reqID)
		return
	}

	// Non-blocking send: the buffer is 1, but if the caller already
	// gave up we don't want to leak this goroutine on a stale channel.
	select {
	case ch <- suggestResult{suggestions: suggestions}:
	default:
	}
}

func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan suggestResult)
	c.pendingMu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- suggestResult{err: err}:
		default:
		}
	}
}

// triggerReconnect starts the reconnect goroutine if one isn't already
// running. The CAS makes this safe under contention.
func (c *Client) triggerReconnect() {
	if !c.reconnectInFlight.CompareAndSwap(false, true) {
		return
	}
	go c.reconnectLoop()
}

func (c *Client) reconnectLoop() {
	defer c.reconnectInFlight.Store(false)

	for attempt := 1; attempt <= c.cfg.MaxReconnectAttempts; attempt++ {
		delay := time.Duration(1<<uint(attempt-1)) * c.cfg.ReconnectDelay
		c.log.Info("reconnect scheduled", "attempt", attempt, "delay", delay)

		select {
		case <-time.After(delay):
		case <-c.shutdownCtx.Done():
			return
		}

		ctx, cancel := context.WithTimeout(c.shutdownCtx, c.cfg.DialTimeout)
		err := c.Connect(ctx)
		cancel()
		if err == nil {
			c.log.Info("reconnect succeeded", "attempt", attempt)
			return
		}
		c.log.Error("reconnect failed", "attempt", attempt, "err", err)
	}
	c.log.Error("giving up on reconnect", "max_attempts", c.cfg.MaxReconnectAttempts)
}

// IsConnected reports whether the WebSocket is currently live. Used by
// the /health endpoint.
func (c *Client) IsConnected() bool { return c.connected.Load() }

// --- helpers ---

func isRetryable(err error) bool {
	return errors.Is(err, suggest.ErrTimeout) ||
		errors.Is(err, suggest.ErrUnavailable) ||
		errors.Is(err, suggest.ErrUpstream)
}

// backoff: exponential with full jitter. attempt is 1-indexed.
func backoff(attempt int, base time.Duration) time.Duration {
	max := base * time.Duration(1<<uint(attempt-1))
	return time.Duration(rand.Int64N(int64(max)))
}
