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
	"strings"
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
	ws                *WSClient
	wakeHandler       func([]byte)
	newSessionHandler func([]byte)

	mu       sync.RWMutex
	sessions map[string]*sessionWS // relay_id → session handle
}

// sessionWS is a per-session handle to the shared daemon WebSocket.
type sessionWS struct {
	daemon              *DaemonWS
	relayID             string
	project             string
	agent               string // server-side agent name (e.g. "claude-code")
	localAgent          string // local agent name (e.g. "claude") — used for skill discovery paths
	cwd                 string
	version             string
	ticket              *TicketRef
	idleNotifyAfterSecs int
	startedAt           time.Time     // session start time; reported on session_start so the server can order live sessions (#154)
	roots               []SessionRoot // trusted roots reported at session_start (#119); resent on reconnect
	injectFunc          func([]byte) error
	killFunc            func()
	suggestionFunc      func() string // composer-suggestion tap (#38), nil when no screen / unsupported agent

	// lastTranscriptAt records when this session last sent a transcript frame.
	// Reported to the server on (re)connect so it can restore the idle-timer
	// baseline after a server restart. Guarded by transcriptMu.
	transcriptMu     sync.Mutex
	lastTranscriptAt time.Time

	// Human-readable session name (like a ChatGPT/Claude conversation title).
	// Assigned automatically from the first user message, or explicitly by the
	// user from the phone. Guarded by nameMu.
	nameMu  sync.Mutex
	name    string
	nameSet bool // a name (auto-derived or user-set) has been assigned
}

// markTranscript records that a transcript frame was just sent for this session.
func (sw *sessionWS) markTranscript() {
	sw.transcriptMu.Lock()
	sw.lastTranscriptAt = time.Now()
	sw.transcriptMu.Unlock()
}

// lastTranscript returns when this session last sent a transcript frame (zero
// if none yet).
func (sw *sessionWS) lastTranscript() time.Time {
	sw.transcriptMu.Lock()
	defer sw.transcriptMu.Unlock()
	return sw.lastTranscriptAt
}

// Name returns the session's current display name.
func (sw *sessionWS) Name() string {
	sw.nameMu.Lock()
	defer sw.nameMu.Unlock()
	return sw.name
}

// named reports whether a name has been assigned yet.
func (sw *sessionWS) named() bool {
	sw.nameMu.Lock()
	defer sw.nameMu.Unlock()
	return sw.nameSet
}

// SetName assigns an explicit (user-provided) name and notifies the server.
func (sw *sessionWS) SetName(name string) {
	sw.nameMu.Lock()
	changed := sw.name != name
	sw.name = name
	sw.nameSet = true
	sw.nameMu.Unlock()
	if changed {
		sw.daemon.sendSessionRenamed(sw.relayID, name)
	}
}

// autoName assigns a name derived from the first user message, but only if no
// name has been set yet. Safe to call repeatedly.
func (sw *sessionWS) autoName(name string) {
	if name == "" {
		return
	}
	sw.nameMu.Lock()
	if sw.nameSet {
		sw.nameMu.Unlock()
		return
	}
	sw.name = name
	sw.nameSet = true
	sw.nameMu.Unlock()
	log.Printf("daemon-ws: auto-named session %s: %q", sw.relayID, name)
	sw.daemon.sendSessionRenamed(sw.relayID, name)
}

// maybeAutoName parses a transcript line and, if it is the first user message,
// derives a name from it. No-op once the session already has a name.
func (sw *sessionWS) maybeAutoName(line string) {
	if sw.named() {
		return
	}
	if name := deriveSessionName(line); name != "" {
		sw.autoName(name)
	}
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

// SetNewSessionHandler sets the handler for new_session control messages.
func (d *DaemonWS) SetNewSessionHandler(fn func([]byte)) {
	d.newSessionHandler = fn
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

// SendRequest wraps `data` in a `{type, relay_id: "", data}` envelope, sends
// it over the daemon WS, and waits for a matching response by `request_id`.
// Returns the response payload (raw JSON) or an error on timeout / disconnect.
//
// `data` must be a map or struct that the server's request_id field can echo
// back; the caller is responsible for putting `request_id` inside it.
func (d *DaemonWS) SendRequest(msgType, requestID string, data interface{}, timeout time.Duration) ([]byte, error) {
	if !d.ws.IsConnected() {
		return nil, fmt.Errorf("daemon WebSocket not connected")
	}
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	envelope := map[string]interface{}{
		"type":     msgType,
		"relay_id": "",
		"data":     json.RawMessage(dataBytes),
	}
	envBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}

	ch := d.ws.RegisterPending(requestID)
	defer d.ws.RemovePending(requestID)

	d.ws.SendText(envBytes)

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("%s timed out after %s", msgType, timeout)
	}
}

// StartSession sends a session_start message over the daemon WS and waits
// for the server to acknowledge it. This replaces HTTP enrollment for
// sessions within an already-enrolled daemon.
func (d *DaemonWS) StartSession(relayID, project, agent, cwd, version, name string, ticket *TicketRef, idleNotifyAfterSecs int, startedAt time.Time, roots []SessionRoot) ([]Skill, error) {
	// Store metadata on the session handle so we can re-register on reconnect.
	// Use a write lock: reregisterSessions reads these fields after releasing
	// d.mu, so writing them without the lock is a data race.
	d.mu.Lock()
	sw := d.sessions[relayID]
	if sw != nil {
		sw.project = project
		sw.agent = agent
		sw.cwd = cwd
		sw.version = version
		sw.ticket = ticket
		sw.idleNotifyAfterSecs = idleNotifyAfterSecs
		sw.startedAt = startedAt
		sw.roots = roots
	}
	d.mu.Unlock()

	// Initial start has no transcripts yet, so report 0 (omitted on the wire).
	return d.sendSessionStart(relayID, project, agent, cwd, version, name, ticket, idleNotifyAfterSecs, 0, startedAt, roots)
}

// sendSessionStart sends a session_start message and waits for ack. Returns
// the skill bundle the server delivered for this session (may be empty).
// lastTranscriptUnix, when > 0, tells the server when this session last sent a
// transcript so it can restore the idle-timer baseline after a restart.
func (d *DaemonWS) sendSessionStart(relayID, project, agent, cwd, version, name string, ticket *TicketRef, idleNotifyAfterSecs int, lastTranscriptUnix int64, startedAt time.Time, roots []SessionRoot) ([]Skill, error) {
	hostname, _ := os.Hostname()
	data := map[string]interface{}{
		"project": project,
		"agent":   agent,
		"cwd":     cwd,
		"version": version,
		"name":    name,
	}
	if !startedAt.IsZero() {
		data["started_at"] = startedAt.UTC().Format(time.RFC3339)
	}
	if ticket != nil {
		data["ticket"] = ticket
	}
	if idleNotifyAfterSecs > 0 {
		data["idle_notify_after_secs"] = idleNotifyAfterSecs
	}
	if len(roots) > 0 {
		data["roots"] = roots
	}
	if lastTranscriptUnix > 0 {
		data["last_transcript_at"] = lastTranscriptUnix
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
		return nil, err
	}

	// Register a pending response keyed by relay_id so we can wait for the ack
	ch := d.ws.RegisterPending("session_start:" + relayID)
	defer d.ws.RemovePending("session_start:" + relayID)

	d.ws.SendText(msgBytes)

	select {
	case resp := <-ch:
		var ack struct {
			Type    string  `json:"type"`
			Project string  `json:"project"`
			Error   string  `json:"error,omitempty"`
			Skills  []Skill `json:"skills,omitempty"`
		}
		if err := json.Unmarshal(resp, &ack); err == nil && ack.Error != "" {
			return nil, fmt.Errorf("session_start failed: %s", ack.Error)
		}
		return ack.Skills, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("session_start timed out")
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
		// Report the last transcript time so the server can restore the idle
		// timer if it restarted while this session was idle.
		var lastTranscriptUnix int64
		if t := sw.lastTranscript(); !t.IsZero() {
			lastTranscriptUnix = t.Unix()
		}
		// Reconnect re-registers existing sessions; skills are installed once
		// at original session start, so the returned list is discarded here.
		if _, err := d.sendSessionStart(sw.relayID, sw.project, sw.agent, sw.cwd, sw.version, sw.Name(), sw.ticket, sw.idleNotifyAfterSecs, lastTranscriptUnix, sw.startedAt, sw.roots); err != nil {
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

// dispatchControlPayload routes a decoded control payload through
// routeControlFrame, injecting the session's relay_id if the payload omits it
// (control frames wrapped for a specific session carry relay_id on the
// envelope, not inside the payload). Unrecognized control types are logged and
// dropped by routeControlFrame's default case — never injected into the PTY.
func (d *DaemonWS) dispatchControlPayload(decoded []byte, relayID string) {
	if len(decoded) == 0 || decoded[0] != '{' {
		log.Printf("daemon-ws: dropping non-JSON control payload for %s", relayID)
		return
	}
	var full map[string]interface{}
	if json.Unmarshal(decoded, &full) != nil {
		log.Printf("daemon-ws: dropping unparseable control payload for %s", relayID)
		return
	}
	if _, ok := full["relay_id"]; !ok {
		full["relay_id"] = relayID
	}
	tagged, err := json.Marshal(full)
	if err != nil {
		return
	}
	d.routeControlFrame(tagged)
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
	case "new_session":
		if d.newSessionHandler != nil {
			d.newSessionHandler(data)
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
	case "list_skills":
		d.handleListSkills(data)
	case "suggestion_get":
		go d.handleSuggestionGet(data)
	case "list_tickets":
		go d.handleListTickets(data)
	case "read_ticket":
		go d.handleReadTicket(data)
	case "create_ticket":
		go d.handleCreateTicket(data)
	case "update_ticket":
		go d.handleUpdateTicket(data)
	case "config_get":
		d.handleConfigGet(data)
	case "config_set":
		d.handleConfigSet(data)
	case "set_session_name":
		d.handleSetSessionName(data)
	default:
		log.Printf("daemon-ws: unknown control message: %s", msg.Type)
	}
}

// sendSessionRenamed notifies the server that a session's name changed.
func (d *DaemonWS) sendSessionRenamed(relayID, name string) {
	msg := map[string]string{
		"type":     "session_renamed",
		"relay_id": relayID,
		"name":     name,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	d.ws.SendText(data)
}

// sendSessionIdleConfig tells the server that a live session's resolved idle
// threshold changed, so it can re-arm (or cancel) the idle timer. The server
// otherwise only learns the threshold at session_start, so a mid-session config
// edit wouldn't take effect until the session restarts or reconnects.
func (d *DaemonWS) sendSessionIdleConfig(relayID string, idleNotifyAfterSecs int) {
	data, err := json.Marshal(map[string]int{"idle_notify_after_secs": idleNotifyAfterSecs})
	if err != nil {
		return
	}
	msg := map[string]interface{}{
		"type":     "session_idle_config",
		"relay_id": relayID,
		"data":     json.RawMessage(data),
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}
	d.ws.SendText(msgBytes)
}

// sendSessionAwaitUser tells the server that a live session has handed control
// back to a human (a ticket handoff: submit/approve/reject/merge/close). The
// server marks the session "waiting" and suppresses its idle push until the user
// re-engages. Session-scoped, fire-and-forget, no reply — modelled on
// sendSessionIdleConfig.
func (d *DaemonWS) sendSessionAwaitUser(relayID string) {
	msg := map[string]interface{}{
		"type":     "session_await_user",
		"relay_id": relayID,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}
	d.ws.SendText(msgBytes)
}

// sendTicketMerged tells the server that a greenlight agent merged a ticket via
// `greenlight ticket merge`, so it can tag a reserved `done` (issue #159).
// Device-scoped (no relay_id), fire-and-forget, no reply — modelled on
// sendSessionAwaitUser. Best-effort: the merge already happened, so a send
// failure must not surface to the user.
func (d *DaemonWS) sendTicketMerged(provider, repoKey, opaqueID string) {
	msg := map[string]interface{}{
		"type":      "ticket_merged",
		"provider":  provider,
		"repo_key":  repoKey,
		"opaque_id": opaqueID,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}
	d.ws.SendText(msgBytes)
}

// SendSessionRoots pushes a mid-session trusted-root update to the server (#119,
// §4.1). The set fully replaces the session's prior non-cwd roots, so it's
// idempotent — a missed or duplicated send self-heals on the next one. Also
// updates the stored set so a later reconnect re-reports the latest roots.
func (d *DaemonWS) SendSessionRoots(relayID string, roots []SessionRoot) {
	d.mu.Lock()
	if sw := d.sessions[relayID]; sw != nil {
		sw.roots = roots
	}
	d.mu.Unlock()

	data, err := json.Marshal(map[string]interface{}{"roots": roots})
	if err != nil {
		return
	}
	msg := map[string]interface{}{
		"type":     "session_roots",
		"relay_id": relayID,
		"data":     json.RawMessage(data),
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}
	d.ws.SendText(msgBytes)
}

// liveSessionProjects returns a relay_id → project snapshot of every live
// session. Used to recompute resolved config per session after a config write.
func (d *DaemonWS) liveSessionProjects() map[string]string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]string, len(d.sessions))
	for id, sw := range d.sessions {
		out[id] = sw.project
	}
	return out
}

// sessionName returns the in-memory name for a live session, or "".
func (d *DaemonWS) sessionName(relayID string) string {
	d.mu.RLock()
	sw := d.sessions[relayID]
	d.mu.RUnlock()
	if sw == nil {
		return ""
	}
	return sw.Name()
}

// handleSetSessionName applies a name change requested from the phone. It works
// for both live sessions (in-memory handle) and completed sessions (on-disk
// record), then echoes session_renamed back to the server.
func (d *DaemonWS) handleSetSessionName(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
		Name    string `json:"name"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		log.Printf("daemon-ws: set_session_name missing relay_id")
		return
	}
	name := sanitizeSessionName(msg.Name)

	// Persist to the on-disk record if one exists. Live sessions have no record
	// until they end, so this is a no-op for them (the name is held in memory
	// and written when saveSessionRecord runs at session exit).
	updateSessionRecordName(msg.RelayID, name)

	d.mu.RLock()
	sw := d.sessions[msg.RelayID]
	d.mu.RUnlock()
	if sw != nil {
		sw.SetName(name) // updates the live handle and sends session_renamed
		return
	}
	// Completed session — no live handle, so echo the rename directly.
	d.sendSessionRenamed(msg.RelayID, name)
	log.Printf("daemon-ws: renamed completed session %s: %q", msg.RelayID, name)
}

// sanitizeSessionName normalizes a user-provided session name: single-line,
// trimmed, length-capped.
func sanitizeSessionName(name string) string {
	name = strings.ReplaceAll(name, "\r", " ")
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.TrimSpace(name)
	const maxLen = 80
	if len(name) > maxLen {
		name = strings.TrimSpace(name[:maxLen])
	}
	return name
}

// deriveSessionName extracts a short title from a transcript line if it is a
// genuine first user message. Returns "" for anything else (assistant turns,
// tool results, system/command-injected messages).
func deriveSessionName(line string) string {
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &entry) != nil || entry.Type != "user" {
		return ""
	}
	if len(entry.Message.Content) == 0 {
		return ""
	}
	// Content is either a plain string or an array of typed blocks.
	var text string
	if json.Unmarshal(entry.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &blocks) != nil {
			return ""
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return "" // a tool result, not a typed user message
			}
			if b.Type == "text" && b.Text != "" {
				if text != "" {
					text += " "
				}
				text += b.Text
			}
		}
	}
	return summarizeSessionName(text)
}

// summarizeSessionName turns a user message into a title: its first line,
// length-capped. The device truncates to the row width for display, so the cap
// only exists to bound transport (the name rides every session_start and the
// full session_history_response replay) — keep it generous.
func summarizeSessionName(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Skip system context / command-injected / interrupted-turn messages.
	if strings.HasPrefix(text, "<") || strings.HasPrefix(text, "[Request interrupted") {
		return ""
	}
	// First line only — keeps a multi-paragraph prompt's later lines out of the
	// title.
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	if text == "" {
		return ""
	}
	// Cap by rune count (not bytes, so we never split a multi-byte character),
	// then trim back to the last word boundary so we don't cut mid-word.
	const maxLen = 120
	if r := []rune(text); len(r) > maxLen {
		text = string(r[:maxLen])
		if i := strings.LastIndexByte(text, ' '); i > 0 {
			text = text[:i]
		}
		text = strings.TrimSpace(text)
	}
	return text
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
	if json.Unmarshal(data, &msg) != nil {
		return false
	}

	// Device-scoped control frames have no relay_id (no session yet).
	// Dispatch them directly to routeControlFrame.
	if msg.RelayID == "" {
		if msg.Type == "new_session" || msg.Type == "list_tickets" || msg.Type == "read_ticket" || msg.Type == "create_ticket" || msg.Type == "update_ticket" || msg.Type == "config_get" || msg.Type == "config_set" {
			d.routeControlFrame(data)
			return true
		}
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

	// Disambiguate PTY input from control frames. The server tags the envelope
	// by frame kind (msg.Type): "input" is verbatim PTY input — inject it even
	// when the user's own message happens to be JSON; "control" is a control
	// frame — route it, never inject. routeControlFrame's default case safely
	// logs+drops anything this build doesn't recognize, so a control message
	// from a newer server can never be typed into the agent as keystrokes.
	switch msg.Type {
	case "control":
		d.dispatchControlPayload(decoded, msg.RelayID)
		return true
	case "input":
		// fall through to injection below — no content inspection.
	default:
		// Legacy servers tag both input and control as "binary", so they are
		// indistinguishable at the envelope level. Fall back to content
		// inspection: a JSON object with a non-empty top-level "type" is a
		// control frame; anything else is input. This preserves the keystroke
		// safety net against older servers, at the cost of dropping the rare
		// phone message that is itself a {"type":...} object.
		if len(decoded) > 0 && decoded[0] == '{' {
			var ctrl struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(decoded, &ctrl) == nil && ctrl.Type != "" {
				d.dispatchControlPayload(decoded, msg.RelayID)
				return true
			}
		}
	}

	log.Printf("daemon-ws: inject %d bytes to %s: %q",
		len(decoded), msg.RelayID, previewBytes(decoded, 60))

	// Deliver through the shared submit path so phone messages and the autopilot
	// initial prompt are typed into the PTY identically.
	if err := submitInput(sw.injectFunc, decoded); err != nil {
		log.Printf("daemon-ws: inject error for %s: %v", msg.RelayID, err)
	}

	return true
}

// submitGap is the delay between injecting a message's text and the separate
// Enter keystroke, so TUIs register a submit rather than a paste.
const submitGap = 50 * time.Millisecond

// submitInput injects raw bytes into a PTY as a user message using the default
// steady-state Enter gap (submitGap). It is the path for phone relay_input
// messages (handleRelayMessage). The autopilot initial prompt uses
// submitInputGap directly with a wider gap (see injectInitialPrompt).
func submitInput(inject func([]byte) error, raw []byte) error {
	return submitInputGap(inject, raw, submitGap)
}

// submitInputGap injects raw bytes into a PTY as a user message: it converts
// newlines to carriage returns (raw PTY mode treats Enter as \r), strips the
// trailing \r, injects the text, then injects a separate \r after gap so TUIs
// treat it as a submit and not a paste. It returns the first inject error. The
// gap is caller-tunable because a freshly rendered TUI composer needs a longer
// beat before the Enter than a session already idling at the prompt.
func submitInputGap(inject func([]byte) error, raw []byte, gap time.Duration) error {
	// Convert \n to \r for raw PTY mode (Enter = \r, not \n).
	input := bytes.ReplaceAll(raw, []byte{'\n'}, []byte{'\r'})

	// Strip trailing \r — send it separately after a brief delay so TUI
	// apps don't treat the whole thing as a paste.
	text := bytes.TrimRight(input, "\r")
	needsSubmit := len(text) < len(input) || len(text) > 0

	if len(text) > 0 {
		if err := inject(text); err != nil {
			return err
		}
	}

	if needsSubmit {
		time.Sleep(gap)
		if err := inject([]byte{'\r'}); err != nil {
			return err
		}
	}

	return nil
}

// previewBytes returns a short, log-friendly preview of `b` for
// diagnostic logging — capped at `max` runes and with control bytes
// rendered as Go escapes (\r, \n, \t, \xNN).
func previewBytes(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	return strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%q", string(b)), `"`), `"`)
}

// handleSessionHistory loads persisted session records and sends them back to the server.
func (d *DaemonWS) handleSessionHistory() {
	records := listSessionRecords()
	if records == nil {
		records = []sessionRecord{}
	}
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

// handleListSkills replies with the names of skills currently installed under
// the session's _greenlight namespace dir. The list is derived by scanning the
// filesystem (not by remembering what was installed), so it reflects the
// current state — including any skills the user manually removed.
func (d *DaemonWS) handleListSkills(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		log.Printf("daemon-ws: list_skills missing relay_id")
		return
	}
	d.mu.RLock()
	sw := d.sessions[msg.RelayID]
	d.mu.RUnlock()
	if sw == nil {
		log.Printf("daemon-ws: list_skills for unknown session %s", msg.RelayID)
		return
	}
	names := listSkills(sw.localAgent, sw.cwd)
	if names == nil {
		names = []string{} // emit [] not null on the wire
	}
	resp := map[string]interface{}{
		"type":     "skills_listed",
		"relay_id": msg.RelayID,
		"skills":   names,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: marshal skills_listed: %v", err)
		return
	}
	d.ws.SendText(out)
}

// handleSuggestionGet greps the live PTY composer line for the agent's
// ghost-suggestion text on demand (experimental, issue #38) and replies
// suggested_prompt. Runs in its own goroutine because the tap samples over
// ~120ms. Replies with empty text when the session has no tap (feature off) or
// is unknown, so a caller is never left waiting. The app polls this while on
// the session transcript screen and offers any non-empty text as an
// autocomplete chip.
func (d *DaemonWS) handleSuggestionGet(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		log.Printf("daemon-ws: suggestion_get missing relay_id")
		return
	}
	d.mu.RLock()
	sw := d.sessions[msg.RelayID]
	d.mu.RUnlock()

	text := ""
	if sw == nil {
		log.Printf("daemon-ws: suggestion_get for unknown session %s", msg.RelayID)
	} else if sw.suggestionFunc != nil {
		// Sessions without a tap (non-claude / unsupported agent) silently
		// return empty; only log an actual extracted suggestion.
		if text = sw.suggestionFunc(); text != "" {
			log.Printf("daemon-ws: suggestion_get %s -> %q", msg.RelayID, text)
		}
	}
	resp := map[string]interface{}{
		"type":     "suggested_prompt",
		"relay_id": msg.RelayID,
		"text":     text,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: marshal suggested_prompt: %v", err)
		return
	}
	d.ws.SendText(out)
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
		// Gemini may self-restart and replace its transcript file with one that
		// has a new session ID, leaving the cached convID stale — prefer a fresh
		// cwd-based scan for it. For other agents, the convID maps deterministically
		// to a single transcript file, so prefer it: a cwd-scan can otherwise return
		// a previous session's transcript when multiple sessions share a directory.
		if sw.agent == "gemini" {
			transcriptPath = deriveTranscriptPath(sw.agent, "", sw.cwd)
			if transcriptPath == "" && convID != "" {
				transcriptPath = deriveTranscriptPath(sw.agent, convID, sw.cwd)
			}
		} else if convID != "" {
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
