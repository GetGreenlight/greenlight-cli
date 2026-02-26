//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"log"
	"math/rand"
	"net/http"
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
	url    string
	token  string
	mode   WSMode
	inject func([]byte) error

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
	for {
		select {
		case <-c.done:
			return
		default:
		}

		connStart := time.Now()
		err := c.connectAndRead()
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
		log.Printf("ws: binary write error: %v", err)
	}
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
			log.Printf("ws: drain write error: %v", err)
			// Re-queue unsent messages (from index i onward).
			unsent := queue[i:]
			c.textMu.Lock()
			// Prepend unsent to any messages that arrived while draining.
			c.textQueue = append(unsent, c.textQueue...)
			if len(c.textQueue) > textQueueSize {
				c.textQueue = c.textQueue[:textQueueSize]
			}
			c.textMu.Unlock()
			return
		}
	}
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

func (c *WSClient) connectAndRead() error {
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

	// Read loop: each message is raw bytes to inject
	for {
		_, data, err := conn.Read(ctx)
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

		if len(data) > 0 && c.mode != WSModeW {
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
