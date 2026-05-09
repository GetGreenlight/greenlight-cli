//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// WSMode controls the directionality of the WebSocket connection.
type WSMode int

const (
	WSModeRW WSMode = iota // read input from server, write output to server (default)
	WSModeR                // read input from server only
	WSModeW                // write output to server only
)

// textQueueSize is the max number of text messages buffered during disconnection.
const textQueueSize = 1024

// WSClient connects to a remote WebSocket server and injects received
// messages into the PTY via the provided inject function. When connected,
// it also sends PTY output back to the server.
type WSClient struct {
	url         string
	token       string
	mode        WSMode
	inject      func([]byte) error
	controlFunc    func([]byte) // optional handler for binary control messages
	textFrameFunc  func([]byte) bool // optional handler for unrouted text frames; returns true if consumed
	reconnectFunc  func() // called after reconnecting (not on first connect)

	done chan struct{}
	wg   sync.WaitGroup

	// Connection for sending output. Protected by connMu.
	connMu sync.Mutex
	conn   *websocket.Conn

	// Buffered text messages (transcript data) that failed to send.
	// Protected by textMu. Messages are queued when conn is nil or
	// a write fails, and drained on reconnection.
	textMu    sync.Mutex
	textQueue [][]byte

	// Pending permission requests waiting for server responses.
	// Maps client-generated request_id → response channel.
	pendingMu sync.Mutex
	pending   map[string]chan []byte
}

// NewWSClient creates a new WebSocket client. Call Run to start connecting.
func NewWSClient(url, token string, mode WSMode, inject func([]byte) error) *WSClient {
	return &WSClient{
		url:    url,
		token:  token,
		mode:   mode,
		inject: inject,
		done:   make(chan struct{}),
	}
}

// Run connects to the WebSocket server and reads messages in a loop.
// On disconnect, it reconnects with exponential backoff.
// Blocks until Close is called.
func (c *WSClient) Run() {
	c.wg.Add(1)
	defer c.wg.Done()

	var attempt int
	firstConnect := true
	for {
		select {
		case <-c.done:
			return
		default:
		}

		connStart := time.Now()
		err := c.connectAndRead(firstConnect)
		firstConnect = false
		if err == nil {
			// Clean shutdown via Close()
			return
		}

		// Reset backoff if the connection lasted more than 60s,
		// so transient failures after a long session start fresh.
		if time.Since(connStart) > 60*time.Second {
			attempt = 0
		}

		select {
		case <-c.done:
			return
		default:
		}

		delay := backoff(attempt)
		log.Printf("ws: disconnected (%v), reconnecting in %v", err, delay)
		attempt++

		select {
		case <-time.After(delay):
		case <-c.done:
			return
		}
	}
}

// Send writes PTY output to the remote server as a binary frame. Safe to call
// from any goroutine. Silently drops data if not connected or if mode is read-only.
// The write happens asynchronously so the caller (PTY output relay) is never
// blocked by slow or broken WebSocket connections.
func (c *WSClient) Send(data []byte) {
	if c.mode == WSModeR {
		return
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return
	}

	cp := make([]byte, len(data))
	copy(cp, data)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageBinary, cp); err != nil {
			log.Printf("ws: binary write error: %v", err)
		}
	}()
}

// SendText writes a text frame to the remote server. Used for JSON messages
// (e.g. transcript data). Safe to call from any goroutine. If the connection
// is down or the write fails, the message is queued for retry on reconnection.
func (c *WSClient) SendText(data []byte) {
	if c.mode == WSModeR {
		return
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		log.Printf("ws: SendText queued (no connection), %d bytes", len(data))
		c.enqueueText(data)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		log.Printf("ws: text write error: %v", err)
		c.enqueueText(data)
	}
}

// enqueueText adds a text message to the retry queue. If the queue is full,
// the oldest message is dropped.
func (c *WSClient) enqueueText(data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)

	c.textMu.Lock()
	defer c.textMu.Unlock()

	if len(c.textQueue) >= textQueueSize {
		// Drop the oldest message to make room.
		log.Printf("ws: text queue full (%d), dropping oldest message", textQueueSize)
		c.textQueue = c.textQueue[1:]
	}
	c.textQueue = append(c.textQueue, cp)
}

// drainTextQueue sends all queued text messages over the connection.
// Called after a new connection is established.
func (c *WSClient) drainTextQueue(conn *websocket.Conn) {
	c.textMu.Lock()
	queue := c.textQueue
	c.textQueue = nil
	c.textMu.Unlock()

	if len(queue) == 0 {
		return
	}

	log.Printf("ws: draining %d queued text messages", len(queue))
	for i, msg := range queue {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := conn.Write(ctx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			log.Printf("ws: drain write error after %d/%d messages, dropping %d unsent: %v", i, len(queue), len(queue)-i, err)
			return
		}
	}
}

// RegisterPending creates a channel for receiving a permission response
// for the given client-generated request ID.
func (c *WSClient) RegisterPending(requestID string) <-chan []byte {
	ch := make(chan []byte, 1)
	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = make(map[string]chan []byte)
	}
	c.pending[requestID] = ch
	c.pendingMu.Unlock()
	return ch
}

// RemovePending removes a pending request channel (idempotent).
func (c *WSClient) RemovePending(requestID string) {
	c.pendingMu.Lock()
	delete(c.pending, requestID)
	c.pendingMu.Unlock()
}

// routePermissionResponse checks if a frame is a permission_response
// and routes it to the pending channel. Returns true if handled.
func (c *WSClient) routePermissionResponse(data []byte) bool {
	// Quick check before parsing JSON
	if len(data) < 20 || data[0] != '{' {
		return false
	}
	var msg struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		RelayID   string `json:"relay_id"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return false
	}

	// Route session_started ack by relay_id
	if msg.Type == "session_started" && msg.RelayID != "" {
		key := "session_start:" + msg.RelayID
		c.pendingMu.Lock()
		ch, ok := c.pending[key]
		if ok {
			delete(c.pending, key)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- data
		}
		return true
	}

	// Route control messages to the control handler
	if msg.Type == "session_history" || msg.Type == "session_transcript" || msg.Type == "wake" || msg.Type == "delete_session" {
		if c.controlFunc != nil {
			c.controlFunc(data)
		}
		return true
	}

	// Route any response with a request_id that matches a pending caller.
	// Covers permission_response, secrets_*_response, pubkey_put_response,
	// oauth_providers_response, etc.
	if msg.RequestID == "" {
		if msg.Type == "permission_response" {
			log.Printf("ws: permission_response with empty request_id: %s", string(data))
		}
		return false
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[msg.RequestID]
	if ok {
		delete(c.pending, msg.RequestID)
	}
	c.pendingMu.Unlock()
	if ok {
		ch <- data
		return true
	}
	// No pending caller for this request_id. If it's clearly a response type,
	// swallow it; otherwise let other handlers see it.
	if strings.HasSuffix(msg.Type, "_response") || msg.Type == "permission_response" {
		log.Printf("ws: %s for unknown request_id %s", msg.Type, msg.RequestID)
		return true
	}
	return false
}

// IsConnected returns true if the WebSocket connection is currently active.
func (c *WSClient) IsConnected() bool {
	c.connMu.Lock()
	connected := c.conn != nil
	c.connMu.Unlock()
	return connected
}

// Close signals the client to stop and waits for it to exit.
func (c *WSClient) Close() {
	close(c.done)
	c.wg.Wait()
}

func (c *WSClient) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *WSClient) connectAndRead(firstConnect bool) error {
	// Create a context that cancels when Close() is called,
	// so conn.Read unblocks immediately on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer cancel()

	// Build dial options with optional auth header
	opts := &websocket.DialOptions{}
	if c.token != "" {
		opts.HTTPHeader = http.Header{
			"Authorization": []string{"Bearer " + c.token},
		}
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	conn, _, err := websocket.Dial(dialCtx, c.url, opts)
	if err != nil {
		return err
	}
	defer func() {
		c.setConn(nil)
		conn.CloseNow()
	}()

	c.setConn(conn)
	log.Printf("ws: connected to %s", c.url)

	// Drain any text messages that were queued during disconnection.
	c.drainTextQueue(conn)

	// On reconnect, notify the owner so it can re-register state (e.g. sessions).
	// Run in a goroutine so the read loop below can process ack responses.
	if !firstConnect && c.reconnectFunc != nil {
		go c.reconnectFunc()
	}

	// Read loop: text frames are relay input (PTY injection),
	// binary frames are control messages from the server.
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			// If we're shutting down, report clean exit
			select {
			case <-c.done:
				conn.Close(websocket.StatusNormalClosure, "shutting down")
				return nil
			default:
			}
			return err
		}

		// Check for permission response frames before PTY injection.
		// Try both text and binary — server may use either frame type.
		if c.routePermissionResponse(data) {
			continue
		}

		// Binary frames are control messages — dispatch to controlFunc.
		if msgType == websocket.MessageBinary && len(data) > 0 {
			if c.controlFunc != nil {
				c.controlFunc(data)
			}
			continue
		}

		// Let the text frame handler try first (used by daemon WS to
		// route input to the correct session by relay_id).
		if c.textFrameFunc != nil && c.textFrameFunc(data) {
			continue
		}

		if len(data) > 0 && c.mode != WSModeW && c.inject != nil {
			// In raw mode, Enter is \r (0x0D), not \n (0x0A).
			data = bytes.ReplaceAll(data, []byte{'\n'}, []byte{'\r'})

			// Strip any trailing \r — we'll send it separately below.
			text := bytes.TrimRight(data, "\r")
			needsSubmit := len(text) < len(data) || len(text) > 0

			// Inject the text content first.
			if len(text) > 0 {
				if err := c.inject(text); err != nil {
					log.Printf("ws: inject error: %v", err)
				}
			}

			// Then send \r separately after a brief delay, simulating
			// the user pressing Enter. Sending it in one write with the
			// text can cause TUI apps to treat it as a paste.
			if needsSubmit {
				time.Sleep(50 * time.Millisecond)
				if err := c.inject([]byte{'\r'}); err != nil {
					log.Printf("ws: inject error: %v", err)
				}
			}
		}
	}
}

// backoff returns a duration for the given attempt number.
// Exponential: 1s, 2s, 4s, 8s, 16s, 30s (capped) with ±25% jitter.
func backoff(attempt int) time.Duration {
	const maxDelay = 30 * time.Second
	if attempt > 30 {
		attempt = 30 // prevent integer overflow in shift
	}
	base := time.Second * time.Duration(1<<uint(attempt))
	if base > maxDelay {
		base = maxDelay
	}
	// Add jitter: ±25%
	jitter := time.Duration(float64(base) * (0.5*rand.Float64() - 0.25))
	return base + jitter
}
