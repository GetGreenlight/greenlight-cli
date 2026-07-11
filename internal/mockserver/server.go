// Package mockserver implements a fake greenlight relay server. It is
// shared between the Go integration tests and the standalone
// `greenlight-mockserver` dev binary so the wire protocol stays in one
// place.
//
// The server speaks just enough of the relay protocol to drive a real
// greenlight CLI/daemon end to end:
//
//   - HTTP handlers for the REST endpoints the CLI hits during enrollment
//     and activity reporting (defaults are permissive)
//   - A WebSocket upgrader on /ws/relay and /ws/daemon that ACKs
//     session_start, tracks live sessions by relay_id, and lets callers
//     push frames into them
//
// Tests construct one via New() and call Set* methods directly. The dev
// binary in cmd/mockserver wraps the same Server and exposes its
// session-tracking API over HTTP for ad-hoc scripting from another
// terminal.
package mockserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// RecordedRequest is an HTTP request the server received. Bodies are
// captured up front so handlers can re-read them.
type RecordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

// Session is a live WebSocket connection from a CLI instance. Frames
// received from the client are buffered on Inbox; callers can push frames
// to the client with Send.
type Session struct {
	RelayID string
	Path    string // /ws/relay or /ws/daemon
	conn    *websocket.Conn
	ctx     context.Context

	mu    sync.Mutex
	inbox []json.RawMessage // received text frames in order
}

// Send writes a JSON frame to this session as a text WebSocket message.
func (s *Session) Send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(s.ctx, websocket.MessageText, data)
}

// SendBinary writes a raw binary WebSocket frame (used for control
// messages like {"type":"kill"} which the CLI parses as JSON over a
// binary frame).
func (s *Session) SendBinary(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(s.ctx, websocket.MessageBinary, data)
}

// Inbox returns a copy of all text frames received from the client so
// far. The slice is safe to iterate without holding any lock.
func (s *Session) Inbox() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, len(s.inbox))
	copy(out, s.inbox)
	return out
}

// WaitForFrame polls the inbox for a frame matching the predicate.
// Returns the matched frame or nil on timeout. The poll interval is
// 20ms which keeps tests responsive without burning CPU.
func (s *Session) WaitForFrame(match func(json.RawMessage) bool, timeout time.Duration) json.RawMessage {
	deadline := time.Now().Add(timeout)
	seen := 0
	for {
		s.mu.Lock()
		for ; seen < len(s.inbox); seen++ {
			if match(s.inbox[seen]) {
				m := s.inbox[seen]
				s.mu.Unlock()
				return m
			}
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Server is a programmable mock of the greenlight relay server.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	requests []RecordedRequest
	handlers map[string]http.HandlerFunc
	wsHook   func(w http.ResponseWriter, r *http.Request)
	sessions map[string]*Session // relay_id -> session

	// onFrame, if set, is called for every text frame received from any
	// session. The default WS handler still ACKs session_start; this is a
	// pure observer hook for logging/scripting.
	onFrame func(s *Session, frame json.RawMessage)

	// onSessionStart, if set, is called to build the session_started
	// reply. Useful for tests that need the ack to carry skills, errors,
	// or other negotiated fields. Default returns the minimal ack.
	onSessionStart func(relayID string, startFrame json.RawMessage) any

	// secrets is the set of secret key names returned for device-scoped
	// secrets_list requests. Empty by default; set via SetSecrets.
	secrets []string

	// ticketStages is an in-memory stand-in for the real server's ticket_stages
	// table (one scalar stage per ticket), keyed by "repo_key\x00opaque_id".
	// Backs the device-scoped ticket_stage_get / ticket_stage_set daemon-WS ops.
	ticketStages map[string]string

	// ticketTags is an in-memory stand-in for the real server's ticket_tags
	// table, keyed by "repo_key\x00opaque_id". Backs the device-scoped
	// ticket_tags_get / ticket_tags_set daemon-WS ops.
	ticketTags map[string][]string
}

// SetSecrets sets the secret key names the mock returns for secrets_list
// requests over the daemon WebSocket.
func (s *Server) SetSecrets(names ...string) {
	s.mu.Lock()
	s.secrets = append([]string(nil), names...)
	s.mu.Unlock()
}

// New starts a new mock server on a random port.
func New() *Server {
	srv := &Server{
		handlers: make(map[string]http.HandlerFunc),
		sessions: make(map[string]*Session),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.dispatch)
	srv.Server = httptest.NewServer(mux)
	return srv
}

// dispatch is the single root handler. It branches WebSocket upgrades to
// the WS handler (custom or default), records HTTP requests, and runs
// per-path overrides or the built-in defaults.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ws/relay" || r.URL.Path == "/ws/daemon" {
		s.mu.Lock()
		hook := s.wsHook
		s.mu.Unlock()
		if hook != nil {
			hook(w, r)
			return
		}
		s.defaultWS(w, r)
		return
	}

	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   body,
	})
	h, ok := s.handlers[r.URL.Path]
	s.mu.Unlock()

	if ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
		return
	}

	switch r.URL.Path {
	case "/session/enroll":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"approved":true}`)
	case "/activity", "/transcript":
		w.WriteHeader(200)
	default:
		w.WriteHeader(404)
	}
}

// defaultWS accepts the upgrade, registers the session by relay_id once
// session_start arrives, ACKs session_start, and buffers any further
// frames on the Session.Inbox. It exits when the client closes.
func (s *Server) defaultWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// No timeout on the read loop — sessions can be long-lived. We rely
	// on the client closing or the test/dev binary tearing the server
	// down.
	ctx := context.Background()
	sess := &Session{Path: r.URL.Path, conn: conn, ctx: ctx}

	for {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			if sess.RelayID != "" {
				s.mu.Lock()
				delete(s.sessions, sess.RelayID)
				s.mu.Unlock()
			}
			return
		}
		if mt != websocket.MessageText {
			continue
		}

		var hdr struct {
			Type    string `json:"type"`
			RelayID string `json:"relay_id"`
		}
		if json.Unmarshal(data, &hdr) != nil {
			continue
		}

		// Buffer the frame and register the session on first contact.
		sess.mu.Lock()
		sess.inbox = append(sess.inbox, append(json.RawMessage(nil), data...))
		sess.mu.Unlock()

		if sess.RelayID == "" && hdr.RelayID != "" {
			sess.RelayID = hdr.RelayID
			s.mu.Lock()
			s.sessions[hdr.RelayID] = sess
			hook := s.onFrame
			s.mu.Unlock()
			if hook != nil {
				hook(sess, data)
			}
		} else {
			s.mu.Lock()
			hook := s.onFrame
			s.mu.Unlock()
			if hook != nil {
				hook(sess, data)
			}
		}

		if hdr.Type == "session_start" {
			s.mu.Lock()
			ssHook := s.onSessionStart
			s.mu.Unlock()
			var reply any
			if ssHook != nil {
				reply = ssHook(hdr.RelayID, data)
			} else {
				reply = map[string]any{
					"type":     "session_started",
					"relay_id": hdr.RelayID,
				}
			}
			ack, _ := json.Marshal(reply)
			conn.Write(ctx, websocket.MessageText, ack)
		}

		// Device-scoped secrets_list: reply with the configured key names so
		// the daemon's shim-activation probe resolves quickly.
		if hdr.Type == "secrets_list" {
			var env struct {
				Data struct {
					RequestID string `json:"request_id"`
				} `json:"data"`
			}
			json.Unmarshal(data, &env)
			s.mu.Lock()
			names := append([]string(nil), s.secrets...)
			s.mu.Unlock()
			secrets := make([]map[string]any, 0, len(names))
			for _, n := range names {
				secrets = append(secrets, map[string]any{"key_name": n})
			}
			resp, _ := json.Marshal(map[string]any{
				"type":       "secrets_list_response",
				"request_id": env.Data.RequestID,
				"secrets":    secrets,
			})
			conn.Write(ctx, websocket.MessageText, resp)
		}

		// Device-scoped secrets_get: reply not_found by default so callers
		// (e.g. a command shim falling back) fail fast rather than blocking
		// on the request timeout. Tests that exercise real decryption can
		// override via SetWSHandler.
		if hdr.Type == "secrets_get" {
			var env struct {
				Data struct {
					RequestID string `json:"request_id"`
				} `json:"data"`
			}
			json.Unmarshal(data, &env)
			resp, _ := json.Marshal(map[string]any{
				"type":       "secrets_get_response",
				"request_id": env.Data.RequestID,
				"error":      "not_found",
			})
			conn.Write(ctx, websocket.MessageText, resp)
		}

		// Device-scoped secrets_put / secrets_delete: maintain the in-memory
		// name set (the same one secrets_list serves) so command tests can
		// round-trip keygen/set → list → rm. Ciphertext is not retained.
		if hdr.Type == "secrets_put" || hdr.Type == "secrets_delete" {
			var env struct {
				Data struct {
					RequestID string `json:"request_id"`
					Key       string `json:"key"`
				} `json:"data"`
			}
			json.Unmarshal(data, &env)
			respType := "secrets_put_response"
			s.mu.Lock()
			if hdr.Type == "secrets_put" {
				found := false
				for _, n := range s.secrets {
					if n == env.Data.Key {
						found = true
						break
					}
				}
				if !found {
					s.secrets = append(s.secrets, env.Data.Key)
				}
			} else {
				respType = "secrets_delete_response"
				kept := s.secrets[:0]
				for _, n := range s.secrets {
					if n != env.Data.Key {
						kept = append(kept, n)
					}
				}
				s.secrets = kept
			}
			s.mu.Unlock()
			resp, _ := json.Marshal(map[string]any{
				"type":       respType,
				"request_id": env.Data.RequestID,
				"status":     "ok",
			})
			conn.Write(ctx, websocket.MessageText, resp)
		}

		// Device-scoped ticket-stage ops: a small in-memory scalar store
		// standing in for the real server's ticket_stages table. An empty
		// stage on a set clears it.
		if hdr.Type == "ticket_stage_get" || hdr.Type == "ticket_stage_set" {
			var env struct {
				Data struct {
					RequestID string `json:"request_id"`
					RepoKey   string `json:"repo_key"`
					OpaqueID  string `json:"opaque_id"`
					Stage     string `json:"stage"`
				} `json:"data"`
			}
			json.Unmarshal(data, &env)
			key := env.Data.RepoKey + "\x00" + env.Data.OpaqueID
			respType := "ticket_stage_get_response"
			s.mu.Lock()
			if s.ticketStages == nil {
				s.ticketStages = map[string]string{}
			}
			if hdr.Type == "ticket_stage_set" {
				respType = "ticket_stage_set_response"
				if env.Data.Stage == "" {
					delete(s.ticketStages, key)
				} else {
					s.ticketStages[key] = env.Data.Stage
				}
			}
			stage := s.ticketStages[key]
			s.mu.Unlock()
			resp, _ := json.Marshal(map[string]any{
				"type":       respType,
				"request_id": env.Data.RequestID,
				"stage":      stage,
			})
			conn.Write(ctx, websocket.MessageText, resp)
		}

		// Device-scoped ticket-tag ops: a small in-memory replace-set store
		// standing in for the real server's ticket_tags table.
		if hdr.Type == "ticket_tags_get" || hdr.Type == "ticket_tags_set" {
			var env struct {
				Data struct {
					RequestID string   `json:"request_id"`
					RepoKey   string   `json:"repo_key"`
					OpaqueID  string   `json:"opaque_id"`
					Tags      []string `json:"tags"`
				} `json:"data"`
			}
			json.Unmarshal(data, &env)
			key := env.Data.RepoKey + "\x00" + env.Data.OpaqueID
			respType := "ticket_tags_get_response"
			s.mu.Lock()
			if s.ticketTags == nil {
				s.ticketTags = map[string][]string{}
			}
			if hdr.Type == "ticket_tags_set" {
				respType = "ticket_tags_set_response"
				tags := env.Data.Tags
				if tags == nil {
					tags = []string{}
				}
				s.ticketTags[key] = tags
			}
			tags := s.ticketTags[key]
			if tags == nil {
				tags = []string{}
			}
			s.mu.Unlock()
			resp, _ := json.Marshal(map[string]any{
				"type":       respType,
				"request_id": env.Data.RequestID,
				"tags":       tags,
			})
			conn.Write(ctx, websocket.MessageText, resp)
		}
	}
}

// SetHandler installs a per-path HTTP handler override. The default
// handlers for /session/enroll, /activity, /transcript are replaced for
// matching paths.
func (s *Server) SetHandler(path string, h http.HandlerFunc) {
	s.mu.Lock()
	s.handlers[path] = h
	s.mu.Unlock()
}

// SetWSHandler replaces the default WebSocket handler. Use this when a
// test needs to drive the upgrade itself; otherwise rely on the default
// handler plus Send/Sessions.
func (s *Server) SetWSHandler(h func(w http.ResponseWriter, r *http.Request)) {
	s.mu.Lock()
	s.wsHook = h
	s.mu.Unlock()
}

// SetFrameHook registers a callback invoked for every text frame received
// from any session. Useful for live logging in the dev binary.
func (s *Server) SetFrameHook(fn func(s *Session, frame json.RawMessage)) {
	s.mu.Lock()
	s.onFrame = fn
	s.mu.Unlock()
}

// SetSessionStartHook customizes the session_started reply. The hook is
// passed the relay_id and the original session_start frame and must
// return any value that JSON-marshals to a valid ack (typically a
// map[string]any with `type`, `relay_id`, optional `skills`, `error`,
// etc.). Pass nil to restore the default minimal ack.
func (s *Server) SetSessionStartHook(fn func(relayID string, startFrame json.RawMessage) any) {
	s.mu.Lock()
	s.onSessionStart = fn
	s.mu.Unlock()
}

// ClearHandlers resets all per-path handlers, the WS hook, the frame
// hook, the session_start hook, and the recorded request log. Live
// sessions are NOT closed.
func (s *Server) ClearHandlers() {
	s.mu.Lock()
	s.handlers = make(map[string]http.HandlerFunc)
	s.wsHook = nil
	s.onFrame = nil
	s.onSessionStart = nil
	s.requests = nil
	s.mu.Unlock()
}

// Requests returns recorded requests for a single path.
func (s *Server) Requests(path string) []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecordedRequest
	for _, r := range s.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// AllRequests returns a snapshot of every recorded request.
func (s *Server) AllRequests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// Session returns the live session for a relay_id, or nil.
func (s *Server) Session(relayID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[relayID]
}

// Sessions returns all live sessions.
func (s *Server) Sessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

// WaitForSession blocks until a session with the given relay_id is
// registered, or the timeout elapses. Returns nil on timeout.
func (s *Server) WaitForSession(relayID string, timeout time.Duration) *Session {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess := s.Session(relayID); sess != nil {
			return sess
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// WSURL returns ws://host:port/ws/relay for use in -ldflags.
func (s *Server) WSURL() string {
	return "ws://" + s.Listener.Addr().String() + "/ws/relay"
}

// BaseURL returns http://host:port.
func (s *Server) BaseURL() string {
	return "http://" + s.Listener.Addr().String()
}
