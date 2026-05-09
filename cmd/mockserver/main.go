// greenlight-mockserver is a standalone fake of the greenlight relay
// server. Build a dev greenlight binary against its address and you can
// drive a real CLI/daemon end to end without touching the production
// service.
//
// Usage:
//
//	greenlight-mockserver --addr 127.0.0.1:7777
//
// Then in another terminal, build and run greenlight against it:
//
//	go build -ldflags "-X main.wsURL=ws://127.0.0.1:7777/ws/relay" \
//	         -o greenlight-dev .
//	GREENLIGHT_DAEMON_SOCK=$TMPDIR/gl-dev.sock ./greenlight-dev daemon start --foreground
//
// The mockserver listens on `--addr` for both the relay protocol and an
// admin HTTP API used to inspect and drive live sessions:
//
//	GET  /_admin/sessions
//	GET  /_admin/sessions/{relay_id}/inbox
//	POST /_admin/sessions/{relay_id}/send         (text frame)
//	POST /_admin/sessions/{relay_id}/send_binary  (binary frame, used for kill)
//	GET  /_admin/requests
//
// All admin endpoints accept/return JSON. The send endpoints take an
// arbitrary JSON object as the request body and forward it verbatim to
// the session.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"greenlight/internal/mockserver"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "listen address for relay + admin")
	verbose := flag.Bool("v", false, "log every text frame received from clients")
	flag.Parse()

	srv := mockserver.New()
	defer srv.Close()

	if *verbose {
		srv.SetFrameHook(func(s *mockserver.Session, frame json.RawMessage) {
			log.Printf("← [%s %s] %s", s.Path, s.RelayID, string(frame))
		})
	}

	// httptest.NewServer picked a random port; we want a fixed one. Wrap
	// the same handler on the user-specified addr.
	mux := http.NewServeMux()
	mux.Handle("/_admin/", adminHandler(srv))
	mux.Handle("/", srv.Server.Config.Handler)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	go func() {
		if err := http.Serve(ln, mux); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	log.Printf("mockserver listening on %s", ln.Addr())
	log.Printf("  ws://%s/ws/relay", ln.Addr())
	log.Printf("  http://%s/_admin/sessions", ln.Addr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
}

// adminHandler routes /_admin/* requests against the mock server.
func adminHandler(srv *mockserver.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/_admin/")
		switch {
		case path == "sessions" && r.Method == "GET":
			listSessions(w, srv)
		case path == "requests" && r.Method == "GET":
			listRequests(w, srv)
		case strings.HasPrefix(path, "sessions/"):
			sessionRoute(w, r, srv, strings.TrimPrefix(path, "sessions/"))
		default:
			http.NotFound(w, r)
		}
	})
}

func listSessions(w http.ResponseWriter, srv *mockserver.Server) {
	type entry struct {
		RelayID string `json:"relay_id"`
		Path    string `json:"path"`
	}
	out := []entry{}
	for _, s := range srv.Sessions() {
		out = append(out, entry{RelayID: s.RelayID, Path: s.Path})
	}
	writeJSON(w, out)
}

func listRequests(w http.ResponseWriter, srv *mockserver.Server) {
	type entry struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}
	all := srv.AllRequests()
	out := make([]entry, len(all))
	for i, r := range all {
		out[i] = entry{Method: r.Method, Path: r.Path, Body: string(r.Body)}
	}
	writeJSON(w, out)
}

// sessionRoute handles /_admin/sessions/{relay_id}/{action}. Splits the
// remaining path, looks up the session, and dispatches.
func sessionRoute(w http.ResponseWriter, r *http.Request, srv *mockserver.Server, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	relayID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	sess := srv.Session(relayID)
	if sess == nil {
		http.Error(w, "session not found", 404)
		return
	}
	switch {
	case action == "inbox" && r.Method == "GET":
		writeJSON(w, sess.Inbox())
	case action == "send" && r.Method == "POST":
		forwardFrame(w, r, sess, false)
	case action == "send_binary" && r.Method == "POST":
		forwardFrame(w, r, sess, true)
	default:
		http.NotFound(w, r)
	}
}

// forwardFrame reads the request body as a JSON value and pushes it to
// the session as either a text or binary WebSocket frame.
func forwardFrame(w http.ResponseWriter, r *http.Request, sess *mockserver.Session, binary bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), 400)
		return
	}
	send := sess.Send
	if binary {
		send = sess.SendBinary
	}
	if err := send(v); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
