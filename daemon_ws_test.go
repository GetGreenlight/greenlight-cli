//go:build darwin || linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
)

// wrapDaemon builds the daemon-WS text envelope the server uses for PTY input
// and control messages: {"relay_id", "type":kind, "data":b64}. kind is "input"
// or "control" on current servers, or "binary" on legacy servers (which tag
// both alike and force the CLI's content-inspection fallback).
func wrapDaemon(t *testing.T, kind, relayID string, payload []byte) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"relay_id": relayID,
		"type":     kind,
		"data":     base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newRecordingSession registers a session whose inject/kill callbacks record
// what they receive, returning the DaemonWS and accessors guarded by a mutex.
func newRecordingSession(relayID string) (d *DaemonWS, injected func() [][]byte, killedFn func() bool) {
	d = &DaemonWS{sessions: make(map[string]*sessionWS)}
	var mu sync.Mutex
	var inj [][]byte
	killed := false
	d.sessions[relayID] = &sessionWS{
		daemon:  d,
		relayID: relayID,
		injectFunc: func(b []byte) error {
			mu.Lock()
			inj = append(inj, append([]byte(nil), b...))
			mu.Unlock()
			return nil
		},
		killFunc: func() {
			mu.Lock()
			killed = true
			mu.Unlock()
		},
	}
	return d, func() [][]byte {
			mu.Lock()
			defer mu.Unlock()
			out := make([][]byte, len(inj))
			copy(out, inj)
			return out
		}, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return killed
		}
}

func joinBytes(chunks [][]byte) string {
	var b []byte
	for _, c := range chunks {
		b = append(b, c...)
	}
	return string(b)
}

// TestHandleTextFrame_InputTagInjectedVerbatim confirms an "input"-tagged frame
// is injected verbatim even when the user's own message is JSON with a "type"
// field — i.e. intentional JSON messages reach the agent, not dropped.
func TestHandleTextFrame_InputTagInjectedVerbatim(t *testing.T) {
	d, injected, _ := newRecordingSession("r1")

	msg := `{"type":"user","name":"bob"}`
	frame := wrapDaemon(t, "input", "r1", []byte(msg))
	if !d.handleTextFrame(frame) {
		t.Fatal("frame should be consumed")
	}
	got := joinBytes(injected())
	// Trailing \r is sent separately as the submit key.
	if got != msg && got != msg+"\r" {
		t.Fatalf("intentional JSON message not injected verbatim: got %q want %q", got, msg)
	}
}

// TestHandleTextFrame_ControlTagRouted confirms a "control"-tagged frame is
// routed and never injected, regardless of whether this build recognizes the
// inner type.
func TestHandleTextFrame_ControlTagRouted(t *testing.T) {
	// Known control (kill) reaches its handler.
	d, injected, killed := newRecordingSession("r1")
	if !d.handleTextFrame(wrapDaemon(t, "control", "r1", []byte(`{"type":"kill"}`))) {
		t.Fatal("frame should be consumed")
	}
	if !killed() {
		t.Fatal("kill control frame did not reach killFunc")
	}
	if len(injected()) != 0 {
		t.Fatalf("control frame was injected: %q", injected())
	}

	// Unknown control (from a newer server) is dropped, never injected.
	d2, injected2, _ := newRecordingSession("r1")
	if !d2.handleTextFrame(wrapDaemon(t, "control", "r1", []byte(`{"type":"some_future_control"}`))) {
		t.Fatal("frame should be consumed")
	}
	if len(injected2()) != 0 {
		t.Fatalf("unknown control frame was injected: %q", injected2())
	}
}

// TestHandleTextFrame_LegacyBinaryFallback is the regression guard for the
// keystroke-injection bug (#38) against legacy servers that tag everything
// "binary": an unrecognized control payload must still be dropped, while plain
// text is still injected.
func TestHandleTextFrame_LegacyBinaryFallback(t *testing.T) {
	// Control-shaped payload over a legacy "binary" envelope: not injected.
	d, injected, _ := newRecordingSession("r1")
	if !d.handleTextFrame(wrapDaemon(t, "binary", "r1", []byte(`{"type":"some_future_control"}`))) {
		t.Fatal("frame should be consumed")
	}
	if len(injected()) != 0 {
		t.Fatalf("control-shaped legacy frame was injected: %q", injected())
	}

	// Plain text over a legacy "binary" envelope: injected.
	d2, injected2, _ := newRecordingSession("r1")
	if !d2.handleTextFrame(wrapDaemon(t, "binary", "r1", []byte("write a haiku"))) {
		t.Fatal("frame should be consumed")
	}
	got := joinBytes(injected2())
	if got != "write a haiku" && got != "write a haiku\r" {
		t.Fatalf("plain legacy text not injected: got %q", got)
	}
}
