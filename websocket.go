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

		err := c.connectAndRead()
		if err == nil {
			// Clean shutdown via Close()
			return
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

// Send writes PTY output to the remote server. Safe to call from any
// goroutine. Silently drops data if not connected or if mode is read-only.
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

	conn.Write(ctx, websocket.MessageBinary, data)
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
	base := time.Second * time.Duration(1<<uint(attempt))
	const maxDelay = 30 * time.Second
	if base > maxDelay {
		base = maxDelay
	}
	// Add jitter: ±25%
	jitter := time.Duration(float64(base) * (0.5*rand.Float64() - 0.25))
	return base + jitter
}
