//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// WSConn is the interface used by sessions for WebSocket communication.
// Both *WSClient (direct) and *sessionWS (multiplexed via daemon) implement it.
type WSConn interface {
	SendText(data []byte)
	Send(data []byte)
	RegisterPending(requestID string) <-chan []byte
	RemovePending(requestID string)
	Close()
}

// DaemonWS is a multiplexed WebSocket owned by the daemon. All sessions
// share this single connection. Outgoing text frames are tagged with
// relay_id so the server can route them. Incoming messages are dispatched
// to the correct session by relay_id.
type DaemonWS struct {
	ws          *WSClient
	wakeHandler func([]byte)

	mu       sync.RWMutex
	sessions map[string]*sessionWS // relay_id → session handle
}

// sessionWS is a per-session handle to the shared daemon WebSocket.
type sessionWS struct {
	daemon     *DaemonWS
	relayID    string
	project    string
	agent      string
	cwd        string
	version    string
	injectFunc func([]byte) error
	killFunc   func()
}

// NewDaemonWS creates a multiplexed WebSocket connection.
func NewDaemonWS(url, deviceID string) *DaemonWS {
	d := &DaemonWS{
		sessions: make(map[string]*sessionWS),
	}

	// The inject callback receives text frames from the server (input injection).
	// We override the normal inject path since there's no single PTY to write to.
	d.ws = NewWSClient(url, deviceID, WSModeRW, nil)

	d.ws.controlFunc = func(data []byte) {
		d.routeControlFrame(data)
	}

	// Catch any text frame not matched by routePermissionResponse.
	// The server tags phone input with relay_id so we can route to
	// the correct session's PTY.
	d.ws.textFrameFunc = func(data []byte) bool {
		return d.handleTextFrame(data)
	}

	// On reconnect, re-register all active sessions with the server.
	d.ws.reconnectFunc = func() {
		d.reregisterSessions()
	}

	return d
}

// Run starts the WebSocket connection. Blocks until closed.
func (d *DaemonWS) Run() {
	d.ws.Run()
}

// Close shuts down the WebSocket.
func (d *DaemonWS) Close() {
	d.ws.Close()
}

// IsConnected returns whether the WebSocket is connected.
func (d *DaemonWS) IsConnected() bool {
	return d.ws.IsConnected()
}

// SetWakeHandler sets the handler for wake control messages.
func (d *DaemonWS) SetWakeHandler(fn func([]byte)) {
	d.wakeHandler = fn
}

// RegisterSession creates a per-session handle for the given relay ID.
func (d *DaemonWS) RegisterSession(relayID string, injectFunc func([]byte) error, killFunc func()) *sessionWS {
	sw := &sessionWS{
		daemon:     d,
		relayID:    relayID,
		injectFunc: injectFunc,
		killFunc:   killFunc,
	}
	d.mu.Lock()
	d.sessions[relayID] = sw
	d.mu.Unlock()
	log.Printf("daemon-ws: registered session %s", relayID)
	return sw
}

// UnregisterSession removes a session handle.
func (d *DaemonWS) UnregisterSession(relayID string) {
	d.mu.Lock()
	delete(d.sessions, relayID)
	d.mu.Unlock()
	log.Printf("daemon-ws: unregistered session %s", relayID)
}

// StartSession sends a session_start message over the daemon WS and waits
// for the server to acknowledge it. This replaces HTTP enrollment for
// sessions within an already-enrolled daemon.
func (d *DaemonWS) StartSession(relayID, project, agent, cwd, version string) error {
	// Store metadata on the session handle so we can re-register on reconnect
	d.mu.RLock()
	sw := d.sessions[relayID]
	d.mu.RUnlock()
	if sw != nil {
		sw.project = project
		sw.agent = agent
		sw.cwd = cwd
		sw.version = version
	}

	return d.sendSessionStart(relayID, project, agent, cwd, version)
}

// sendSessionStart sends a session_start message and waits for ack.
func (d *DaemonWS) sendSessionStart(relayID, project, agent, cwd, version string) error {
	hostname, _ := os.Hostname()
	data := map[string]string{
		"project": project,
		"agent":   agent,
		"cwd":     cwd,
		"version": version,
	}
	dataBytes, _ := json.Marshal(data)

	msg := map[string]interface{}{
		"type":     "session_start",
		"relay_id": relayID,
		"hostname": hostname,
		"data":     json.RawMessage(dataBytes),
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// Register a pending response keyed by relay_id so we can wait for the ack
	ch := d.ws.RegisterPending("session_start:" + relayID)
	defer d.ws.RemovePending("session_start:" + relayID)

	d.ws.SendText(msgBytes)

	select {
	case resp := <-ch:
		var ack struct {
			Type    string `json:"type"`
			Project string `json:"project"`
			Error   string `json:"error,omitempty"`
		}
		if json.Unmarshal(resp, &ack) == nil && ack.Error != "" {
			return fmt.Errorf("session_start failed: %s", ack.Error)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("session_start timed out")
	}
}

// reregisterSessions re-sends session_start for all active sessions after a reconnect.
func (d *DaemonWS) reregisterSessions() {
	d.mu.RLock()
	sessions := make([]*sessionWS, 0, len(d.sessions))
	for _, sw := range d.sessions {
		sessions = append(sessions, sw)
	}
	d.mu.RUnlock()

	if len(sessions) == 0 {
		return
	}

	log.Printf("daemon-ws: reconnected, re-registering %d session(s)", len(sessions))
	for _, sw := range sessions {
		if err := d.sendSessionStart(sw.relayID, sw.project, sw.agent, sw.cwd, sw.version); err != nil {
			log.Printf("daemon-ws: failed to re-register session %s: %v", sw.relayID, err)
		} else {
			log.Printf("daemon-ws: re-registered session %s", sw.relayID)
		}
	}
}

// EndSession sends a session_end message over the daemon WS.
func (d *DaemonWS) EndSession(relayID string) {
	msg := map[string]string{
		"type":     "session_end",
		"relay_id": relayID,
	}
	msgBytes, _ := json.Marshal(msg)
	d.ws.SendText(msgBytes)
}

// routeControlFrame handles binary control frames from the server.
func (d *DaemonWS) routeControlFrame(data []byte) {
	var msg struct {
		Type    string `json:"type"`
		RelayID string `json:"relay_id"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	switch msg.Type {
	case "kill":
		d.mu.RLock()
		sw := d.sessions[msg.RelayID]
		d.mu.RUnlock()
		if sw != nil && sw.killFunc != nil {
			log.Printf("daemon-ws: kill session %s", msg.RelayID)
			sw.killFunc()
		}
	case "wake":
		if d.wakeHandler != nil {
			d.wakeHandler(data)
		}
	case "session_history":
		d.handleSessionHistory()
	case "session_transcript":
		d.handleSessionTranscript(data)
	default:
		log.Printf("daemon-ws: unknown control message: %s", msg.Type)
	}
}

// handleTextFrame handles text frames not matched by routePermissionResponse.
// It tries to parse JSON with a relay_id and route the content as PTY input
// to the matching session. Returns true if the frame was consumed.
func (d *DaemonWS) handleTextFrame(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}

	var msg struct {
		Type    string `json:"type"`
		RelayID string `json:"relay_id"`
		Text    string `json:"text"`
		Data    string `json:"data"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		return false
	}

	d.mu.RLock()
	sw := d.sessions[msg.RelayID]
	d.mu.RUnlock()
	if sw == nil || sw.injectFunc == nil {
		log.Printf("daemon-ws: text frame for unknown session %s", msg.RelayID)
		return false
	}

	// Extract text content — server may use "text" or "data" field.
	content := msg.Text
	if content == "" {
		content = msg.Data
	}
	if content == "" {
		log.Printf("daemon-ws: text frame for %s has no text/data field", msg.RelayID)
		return true // consumed but nothing to inject
	}

	// The server base64-encodes the text for the daemon WebSocket.
	// Decode it before injecting into the PTY.
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		// Not base64 — use as-is (plain text fallback).
		decoded = []byte(content)
	}

	// The server wraps both input and control messages as type "binary".
	// After decoding, check if the payload is a known control message
	// (e.g. {"type":"kill"}) and route it instead of injecting as text.
	if len(decoded) > 0 && decoded[0] == '{' {
		var ctrl struct{ Type string `json:"type"` }
		if json.Unmarshal(decoded, &ctrl) == nil {
			switch ctrl.Type {
			case "kill", "wake", "session_history", "session_transcript":
				var full map[string]interface{}
				if json.Unmarshal(decoded, &full) == nil {
					if _, ok := full["relay_id"]; !ok {
						full["relay_id"] = msg.RelayID
					}
					if tagged, err := json.Marshal(full); err == nil {
						d.routeControlFrame(tagged)
						return true
					}
				}
			}
		}
	}

	// Convert \n to \r for raw PTY mode (Enter = \r, not \n).
	input := bytes.ReplaceAll(decoded, []byte{'\n'}, []byte{'\r'})

	// Strip trailing \r — send it separately after a brief delay so TUI
	// apps don't treat the whole thing as a paste.
	text := bytes.TrimRight(input, "\r")
	needsSubmit := len(text) < len(input) || len(text) > 0

	if len(text) > 0 {
		if err := sw.injectFunc(text); err != nil {
			log.Printf("daemon-ws: inject error for %s: %v", msg.RelayID, err)
		}
	}

	if needsSubmit {
		time.Sleep(50 * time.Millisecond)
		if err := sw.injectFunc([]byte{'\r'}); err != nil {
			log.Printf("daemon-ws: inject error for %s: %v", msg.RelayID, err)
		}
	}

	return true
}

// handleSessionHistory loads persisted session records and sends them back to the server.
func (d *DaemonWS) handleSessionHistory() {
	records := listSessionRecords()
	log.Printf("daemon-ws: session_history request, returning %d records", len(records))

	resp := map[string]interface{}{
		"type":    "session_history_response",
		"entries": records,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: failed to marshal session history: %v", err)
		return
	}
	d.ws.SendText(data)
}

// handleSessionTranscript loads a transcript file for a session and sends the entries back.
func (d *DaemonWS) handleSessionTranscript(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		log.Printf("daemon-ws: session_transcript missing relay_id")
		d.sendTranscriptResponse("", nil, "missing relay_id")
		return
	}

	convID := lookupConversationID(msg.RelayID)
	if convID == "" {
		log.Printf("daemon-ws: session_transcript: no conversation ID for relay %s", msg.RelayID)
		d.sendTranscriptResponse(msg.RelayID, nil, "no conversation ID found for relay_id")
		return
	}

	rec, err := loadSessionRecord(convID)
	if err != nil {
		log.Printf("daemon-ws: session_transcript: failed to load session record %s: %v", convID, err)
		d.sendTranscriptResponse(msg.RelayID, nil, fmt.Sprintf("session record not found: %v", err))
		return
	}

	transcriptPath := deriveTranscriptPath(rec.Agent, convID, rec.Cwd)
	if transcriptPath == "" {
		log.Printf("daemon-ws: session_transcript: could not derive transcript path for %s", convID)
		d.sendTranscriptResponse(msg.RelayID, nil, "could not derive transcript path")
		return
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		log.Printf("daemon-ws: session_transcript: failed to open %s: %v", transcriptPath, err)
		d.sendTranscriptResponse(msg.RelayID, nil, fmt.Sprintf("transcript file not found: %v", err))
		return
	}
	defer f.Close()

	var entries []json.RawMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Validate it's valid JSON before including
		if json.Valid(line) {
			entries = append(entries, json.RawMessage(append([]byte(nil), line...)))
		}
	}

	log.Printf("daemon-ws: session_transcript for relay %s: %d entries", msg.RelayID, len(entries))
	d.sendTranscriptResponse(msg.RelayID, entries, "")
}

// sendTranscriptResponse sends a session_transcript_response message.
func (d *DaemonWS) sendTranscriptResponse(relayID string, entries []json.RawMessage, message string) {
	resp := map[string]interface{}{
		"type":     "session_transcript_response",
		"relay_id": relayID,
		"entries":  entries,
	}
	if message != "" {
		resp["message"] = message
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: failed to marshal transcript response: %v", err)
		return
	}
	d.ws.SendText(data)
}

// SendText sends a text frame tagged with the session's relay_id.
func (sw *sessionWS) SendText(data []byte) {
	var msg map[string]interface{}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	// For permission requests, relay_id goes inside "data" (server expects it there).
	// For everything else (transcript, cancel), it goes at the top level.
	msgType, _ := msg["type"].(string)
	if msgType == "permission_request" {
		if dataField, ok := msg["data"].(map[string]interface{}); ok {
			dataField["relay_id"] = sw.relayID
		}
	}
	msg["relay_id"] = sw.relayID

	tagged, err := json.Marshal(msg)
	if err != nil {
		return
	}
	sw.daemon.ws.SendText(tagged)
}

// RegisterPending creates a channel for receiving a permission response.
func (sw *sessionWS) RegisterPending(requestID string) <-chan []byte {
	return sw.daemon.ws.RegisterPending(requestID)
}

// RemovePending removes a pending request channel.
func (sw *sessionWS) RemovePending(requestID string) {
	sw.daemon.ws.RemovePending(requestID)
}

// SendWSRequest sends a organization CRUD request to the server over the daemon
// WebSocket and waits for the response. correlationID must be unique per call.
func (d *DaemonWS) SendWSRequest(correlationID, msgType string, data json.RawMessage) ([]byte, error) {
	msg := map[string]interface{}{
		"type":           "ws_request",
		"correlation_id": correlationID,
		"msg_type":       msgType,
	}
	if len(data) > 0 {
		msg["data"] = json.RawMessage(data)
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	ch := d.ws.RegisterPending(correlationID)
	defer d.ws.RemovePending(correlationID)

	d.ws.SendText(msgBytes)

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("ws_request timed out")
	}
}

// Send is a no-op — PTY output is not sent to the server.
func (sw *sessionWS) Send(data []byte) {}

// Close is a no-op — the shared connection is owned by the daemon.
func (sw *sessionWS) Close() {}
