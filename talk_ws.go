//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"nhooyr.io/websocket"
)

// talkWS owns the /ws connection that the talk TUI reads from. Incoming
// messages are pushed into the running tea.Program as typed tea.Msg values.
type talkWS struct {
	url     string
	conn    *websocket.Conn
	program *tea.Program
	ctx     context.Context
	cancel  context.CancelFunc
}

// Tea messages produced by the WS goroutine.
type wsConnectedMsg struct{}
type wsDisconnectedMsg struct{ err string }
type wsMessageMsg struct{ data []byte }

func newTalkWS(userID string) (*talkWS, error) {
	if wsURL == "" {
		return nil, fmt.Errorf("no relay server URL configured")
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("bad relay URL: %w", err)
	}
	// Build-time wsURL points at /ws/relay (or /ws/daemon for daemon mode).
	// The TUI talks to the phone-equivalent /ws endpoint instead.
	u.Path = "/ws"
	q := u.Query()
	q.Set("human_user_id", userID)
	// TODO(secret): /users/register doesn't issue a per-user secret yet,
	// and validateHumanUserSecret on the server returns true for any user
	// whose secret_hash column is null. Sending an empty string keeps us
	// authed for now; once the registration flow stores a real secret
	// we'll need to read it from ~/.greenlight/config and pass it here.
	q.Set("secret", "")
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithCancel(context.Background())
	return &talkWS{
		url:    u.String(),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// run is the WS read loop. It dials the server, then forwards every text
// frame to the tea.Program as a wsMessageMsg until the connection drops or
// the model calls close().
func (w *talkWS) run() {
	dialCtx, dialCancel := context.WithTimeout(w.ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, w.url, nil)
	dialCancel()
	if err != nil {
		w.program.Send(wsDisconnectedMsg{err: err.Error()})
		return
	}
	w.conn = conn
	conn.SetReadLimit(1 << 20) // match the server's 1 MB cap
	w.program.Send(wsConnectedMsg{})

	defer conn.Close(websocket.StatusNormalClosure, "")

	for {
		_, data, err := conn.Read(w.ctx)
		if err != nil {
			if w.ctx.Err() != nil {
				// Closed by us — don't surface as an error.
				return
			}
			w.program.Send(wsDisconnectedMsg{err: err.Error()})
			return
		}
		w.program.Send(wsMessageMsg{data: data})
	}
}

// send writes a JSON text frame back to the server. Used in phase 2+ for
// permission_response and relay_input. Currently unused.
func (w *talkWS) send(data []byte) error {
	if w.conn == nil {
		return fmt.Errorf("not connected")
	}
	return w.conn.Write(w.ctx, websocket.MessageText, data)
}

func (w *talkWS) close() {
	w.cancel()
	if w.conn != nil {
		w.conn.Close(websocket.StatusNormalClosure, "")
	}
}
