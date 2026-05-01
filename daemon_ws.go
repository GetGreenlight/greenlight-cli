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
	case "delete_session":
		d.handleDeleteSession(data)
	case "history_entry":
		d.handleHistoryEntry(data)
	case "project_history":
		d.handleProjectHistory(data)
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
			case "kill", "wake", "session_history", "session_transcript", "delete_session":
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

// handleDeleteSession removes a persisted session record by relay_id.
func (d *DaemonWS) handleDeleteSession(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		log.Printf("daemon-ws: delete_session missing relay_id")
		return
	}
	rec, err := loadSessionRecordByRelayID(msg.RelayID)
	if err != nil {
		log.Printf("daemon-ws: delete_session: no record for relay %s: %v", msg.RelayID, err)
		return
	}
	removeSessionRecord(rec.ConversationID)
	log.Printf("daemon-ws: deleted session record %s (relay %s)", rec.ConversationID, msg.RelayID)
}

// handleHistoryEntry stores a permission request outcome from the server.
func (d *DaemonWS) handleHistoryEntry(data []byte) {
	var msg struct {
		Project string       `json:"project"`
		Entry   historyEntry `json:"entry"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Project == "" {
		return
	}
	appendHistoryEntry(msg.Project, msg.Entry)
}

// handleProjectHistory returns stored history entries for a project.
func (d *DaemonWS) handleProjectHistory(data []byte) {
	var msg struct {
		Project string `json:"project"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Project == "" {
		return
	}

	entries := listProjectHistory(msg.Project, 200)
	if entries == nil {
		entries = []historyEntry{}
	}

	resp := map[string]interface{}{
		"type":    "project_history_response",
		"project": msg.Project,
		"entries": entries,
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: failed to marshal project history: %v", err)
		return
	}
	d.ws.SendText(respData)
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

	var transcriptPath string
	var agent string

	convID := lookupConversationID(msg.RelayID)

	// Try live session first (no saved record yet). For live sessions, prefer a
	// fresh cwd-based scan over the cached convID — agents like Gemini may
	// self-restart and replace their transcript file with one that has a new
	// session ID, leaving the cached convID stale.
	d.mu.RLock()
	sw := d.sessions[msg.RelayID]
	d.mu.RUnlock()
	if sw != nil {
		agent = sw.agent
		transcriptPath = deriveTranscriptPath(sw.agent, "", sw.cwd)
		if transcriptPath == "" && convID != "" {
			transcriptPath = deriveTranscriptPath(sw.agent, convID, sw.cwd)
		}
	}

	// Fall back to completed session record
	if transcriptPath == "" && convID != "" {
		if rec, err := loadSessionRecord(convID); err == nil {
			agent = rec.Agent
			transcriptPath = deriveTranscriptPath(rec.Agent, convID, rec.Cwd)
		}
	}

	if transcriptPath == "" && convID == "" {
		log.Printf("daemon-ws: session_transcript: no conversation ID for relay %s", msg.RelayID)
		d.sendTranscriptResponse(msg.RelayID, nil, "no conversation ID found for relay_id")
		return
	}
	if transcriptPath == "" {
		log.Printf("daemon-ws: session_transcript: could not derive transcript path for %s", convID)
		d.sendTranscriptResponse(msg.RelayID, nil, "could not derive transcript path")
		return
	}

	// Gemini transcripts are a single JSON object (not JSONL) — handle separately.
	if agent == "gemini" {
		d.handleGeminiTranscript(msg.RelayID, transcriptPath)
		return
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		log.Printf("daemon-ws: session_transcript: failed to open %s: %v", transcriptPath, err)
		d.sendTranscriptResponse(msg.RelayID, nil, fmt.Sprintf("transcript file not found: %v", err))
		return
	}
	defer f.Close()

	const maxBytes = 8 << 20 // 8 MB — must not exceed server's WS read limit

	// Determine if this agent needs transcript transformation.
	// Non-Claude agents store transcripts in their native format; we must
	// transform each line to Claude-compatible format before sending to the server.
	var transformFn func(string) string
	switch agent {
	case "codex":
		transformFn = transformCodexEventReplay
	case "copilot":
		transformFn = transformCopilotEvent
	case "cursor":
		transformFn = transformCursorEvent
	case "pi":
		transformFn = transformPiEvent
	}

	var entries []json.RawMessage
	var totalBytes int
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if transformFn != nil {
			transformed := transformFn(string(line))
			if transformed == "" {
				continue
			}
			line = []byte(transformed)
		}
		// Validate it's valid JSON before including
		if json.Valid(line) {
			entries = append(entries, json.RawMessage(append([]byte(nil), line...)))
			totalBytes += len(line)
		}
	}

	// Trim oldest entries until the total fits within the size limit.
	// Account for JSON array overhead (wrapper + relay_id + type fields ~100 bytes).
	for len(entries) > 0 && totalBytes > maxBytes-1024 {
		totalBytes -= len(entries[0])
		entries = entries[1:]
	}

	log.Printf("daemon-ws: session_transcript for relay %s agent=%s: %d entries (%d bytes)", msg.RelayID, agent, len(entries), totalBytes)
	d.sendTranscriptResponseWithAgent(msg.RelayID, entries, "", agent)
}

// sendTranscriptResponse sends a session_transcript_response message.
func (d *DaemonWS) sendTranscriptResponse(relayID string, entries []json.RawMessage, message string) {
	d.sendTranscriptResponseWithAgent(relayID, entries, message, "")
}

// sendTranscriptResponseWithAgent sends a session_transcript_response with an agent field.
func (d *DaemonWS) sendTranscriptResponseWithAgent(relayID string, entries []json.RawMessage, message, agent string) {
	resp := map[string]interface{}{
		"type":     "session_transcript_response",
		"relay_id": relayID,
		"entries":  entries,
	}
	if message != "" {
		resp["message"] = message
	}
	if agent != "" {
		resp["agent"] = agent
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: failed to marshal transcript response: %v", err)
		return
	}
	d.ws.SendText(data)
}

// handleGeminiTranscript reads a Gemini JSON transcript file, transforms each
// message to Claude-compatible format, and sends the entries as a transcript response.
func (d *DaemonWS) handleGeminiTranscript(relayID, transcriptPath string) {
	sessionID, messages, err := readGeminiTranscript(transcriptPath)
	if err != nil {
		log.Printf("daemon-ws: session_transcript: failed to read gemini transcript %s: %v", transcriptPath, err)
		d.sendTranscriptResponse(relayID, nil, fmt.Sprintf("transcript file not found: %v", err))
		return
	}

	const maxBytes = 8 << 20
	var entries []json.RawMessage
	var totalBytes int
	for _, raw := range messages {
		transformed := transformGeminiMessage(raw, sessionID)
		for _, entry := range transformed {
			entryBytes, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			entries = append(entries, json.RawMessage(entryBytes))
			totalBytes += len(entryBytes)
		}
	}

	for len(entries) > 0 && totalBytes > maxBytes-1024 {
		totalBytes -= len(entries[0])
		entries = entries[1:]
	}

	log.Printf("daemon-ws: session_transcript for relay %s: %d gemini entries (%d bytes)", relayID, len(entries), totalBytes)
	d.sendTranscriptResponseWithAgent(relayID, entries, "", "gemini")
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

// Send is a no-op — PTY output is not sent to the server.
func (sw *sessionWS) Send(data []byte) {}

// Close is a no-op — the shared connection is owned by the daemon.
func (sw *sessionWS) Close() {}
