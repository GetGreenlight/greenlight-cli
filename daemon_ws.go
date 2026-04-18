//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// WSConn is the interface used by agent instances for WebSocket communication.
// Both *WSClient (direct) and *agentWS (multiplexed via daemon) implement it.
type WSConn interface {
	SendText(data []byte)
	Send(data []byte)
	RegisterPending(requestID string) <-chan []byte
	RemovePending(requestID string)
	Close()
}

// DaemonWS is a multiplexed WebSocket owned by the daemon. All agent instances
// share this single connection. Outgoing text frames are tagged with
// ai_agent_instance_id so the server can route them. Incoming messages are
// dispatched to the correct agent instance by ai_agent_instance_id.
type DaemonWS struct {
	ws *WSClient

	mu        sync.RWMutex
	instances map[string]*agentWS // ai_agent_instance_id → agent instance handle
}

// agentWS is a per-agent-instance handle to the shared daemon WebSocket.
type agentWS struct {
	daemon            *DaemonWS
	aiAgentInstanceID string
	project           string
	agent             string
	cwd               string
	version           string
	injectFunc        func([]byte) error
	killFunc          func()
}

// NewDaemonWS creates a multiplexed WebSocket connection.
func NewDaemonWS(url, humanUserID string) *DaemonWS {
	d := &DaemonWS{
		instances: make(map[string]*agentWS),
	}

	// The inject callback receives text frames from the server (input injection).
	// We override the normal inject path since there's no single PTY to write to.
	d.ws = NewWSClient(url, humanUserID, WSModeRW, nil)

	d.ws.controlFunc = func(data []byte) {
		d.routeControlFrame(data)
	}

	// Catch any text frame not matched by routePermissionResponse.
	// The server tags phone input with ai_agent_instance_id so we can route
	// to the correct agent instance's PTY.
	d.ws.textFrameFunc = func(data []byte) bool {
		return d.handleTextFrame(data)
	}

	// On reconnect, re-register all active agent instances with the server.
	d.ws.reconnectFunc = func() {
		d.reregisterAgentInstances()
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

// RegisterAgentInstance creates a per-instance handle for the given ID.
func (d *DaemonWS) RegisterAgentInstance(id string, injectFunc func([]byte) error, killFunc func()) *agentWS {
	aw := &agentWS{
		daemon:            d,
		aiAgentInstanceID: id,
		injectFunc:        injectFunc,
		killFunc:          killFunc,
	}
	d.mu.Lock()
	d.instances[id] = aw
	d.mu.Unlock()
	log.Printf("daemon-ws: registered agent instance %s", id)
	return aw
}

// UnregisterAgentInstance removes an agent instance handle.
func (d *DaemonWS) UnregisterAgentInstance(id string) {
	d.mu.Lock()
	delete(d.instances, id)
	d.mu.Unlock()
	log.Printf("daemon-ws: unregistered agent instance %s", id)
}

// ConnectAgentInstance sends an agent_instance_connect message over the daemon
// WS and waits for the server to acknowledge it. This replaces HTTP enrollment
// for agent instances within an already-enrolled daemon.
func (d *DaemonWS) ConnectAgentInstance(id, project, agent, cwd, version string) error {
	// Store metadata on the instance handle so we can re-register on reconnect
	d.mu.RLock()
	aw := d.instances[id]
	d.mu.RUnlock()
	if aw != nil {
		aw.project = project
		aw.agent = agent
		aw.cwd = cwd
		aw.version = version
	}

	return d.sendAgentInstanceConnect(id, agent, version)
}

// sendAgentInstanceConnect sends an agent_instance_connect message and waits
// for ack. Server resolves project/cwd from the DB row — we just send the agent
// harness name and client version.
func (d *DaemonWS) sendAgentInstanceConnect(id, agent, version string) error {
	data := map[string]string{
		"agent":   agent,
		"version": version,
	}
	dataBytes, _ := json.Marshal(data)

	msg := map[string]interface{}{
		"type":                 "agent_instance_connect",
		"ai_agent_instance_id": id,
		"data":                 json.RawMessage(dataBytes),
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// Register a pending response keyed by instance ID so we can wait for the ack
	ch := d.ws.RegisterPending("agent_instance_connect:" + id)
	defer d.ws.RemovePending("agent_instance_connect:" + id)

	d.ws.SendText(msgBytes)

	select {
	case resp := <-ch:
		var ack struct {
			Type  string `json:"type"`
			Error string `json:"error,omitempty"`
		}
		if json.Unmarshal(resp, &ack) == nil && ack.Error != "" {
			return fmt.Errorf("agent_instance_connect failed: %s", ack.Error)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("agent_instance_connect timed out")
	}
}

// reregisterAgentInstances re-sends agent_instance_connect for all active
// instances after a reconnect.
func (d *DaemonWS) reregisterAgentInstances() {
	d.mu.RLock()
	instances := make([]*agentWS, 0, len(d.instances))
	for _, aw := range d.instances {
		instances = append(instances, aw)
	}
	d.mu.RUnlock()

	if len(instances) == 0 {
		return
	}

	log.Printf("daemon-ws: reconnected, re-registering %d agent instance(s)", len(instances))
	for _, aw := range instances {
		if err := d.sendAgentInstanceConnect(aw.aiAgentInstanceID, aw.agent, aw.version); err != nil {
			log.Printf("daemon-ws: failed to re-register agent instance %s: %v", aw.aiAgentInstanceID, err)
		} else {
			log.Printf("daemon-ws: re-registered agent instance %s", aw.aiAgentInstanceID)
		}
	}
}

// DisconnectAgentInstance sends an agent_instance_disconnect message over the
// daemon WS.
func (d *DaemonWS) DisconnectAgentInstance(id string) {
	msg := map[string]string{
		"type":                 "agent_instance_disconnect",
		"ai_agent_instance_id": id,
	}
	msgBytes, _ := json.Marshal(msg)
	d.ws.SendText(msgBytes)
}

// routeControlFrame handles binary control frames from the server.
func (d *DaemonWS) routeControlFrame(data []byte) {
	var msg struct {
		Type              string `json:"type"`
		AIAgentInstanceID string `json:"ai_agent_instance_id"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	switch msg.Type {
	case "kill":
		d.mu.RLock()
		aw := d.instances[msg.AIAgentInstanceID]
		d.mu.RUnlock()
		if aw != nil && aw.killFunc != nil {
			log.Printf("daemon-ws: kill agent instance %s", msg.AIAgentInstanceID)
			aw.killFunc()
		}
	case "delete_agent_instance":
		// Drop any local state for this instance — the server has deleted the row.
		d.mu.Lock()
		delete(d.instances, msg.AIAgentInstanceID)
		d.mu.Unlock()
		log.Printf("daemon-ws: deleted agent instance %s", msg.AIAgentInstanceID)
	default:
		log.Printf("daemon-ws: unknown control message: %s", msg.Type)
	}
}

// handleTextFrame handles text frames not matched by routePermissionResponse.
// It tries to parse JSON with an ai_agent_instance_id and route the content as
// PTY input to the matching instance. Returns true if the frame was consumed.
func (d *DaemonWS) handleTextFrame(data []byte) bool {
	if len(data) == 0 || data[0] != '{' {
		return false
	}

	var msg struct {
		Type              string `json:"type"`
		AIAgentInstanceID string `json:"ai_agent_instance_id"`
		Text              string `json:"text"`
		Data              string `json:"data"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.AIAgentInstanceID == "" {
		return false
	}

	d.mu.RLock()
	aw := d.instances[msg.AIAgentInstanceID]
	d.mu.RUnlock()
	if aw == nil || aw.injectFunc == nil {
		log.Printf("daemon-ws: text frame for unknown agent instance %s", msg.AIAgentInstanceID)
		return false
	}

	// Extract text content — server may use "text" or "data" field.
	content := msg.Text
	if content == "" {
		content = msg.Data
	}
	if content == "" {
		log.Printf("daemon-ws: text frame for %s has no text/data field", msg.AIAgentInstanceID)
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
		var ctrl struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(decoded, &ctrl) == nil {
			switch ctrl.Type {
			case "kill", "delete_agent_instance":
				var full map[string]interface{}
				if json.Unmarshal(decoded, &full) == nil {
					if _, ok := full["ai_agent_instance_id"]; !ok {
						full["ai_agent_instance_id"] = msg.AIAgentInstanceID
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
		if err := aw.injectFunc(text); err != nil {
			log.Printf("daemon-ws: inject error for %s: %v", msg.AIAgentInstanceID, err)
		}
	}

	if needsSubmit {
		time.Sleep(50 * time.Millisecond)
		if err := aw.injectFunc([]byte{'\r'}); err != nil {
			log.Printf("daemon-ws: inject error for %s: %v", msg.AIAgentInstanceID, err)
		}
	}

	return true
}

// SendText sends a text frame tagged with the instance's ai_agent_instance_id.
func (aw *agentWS) SendText(data []byte) {
	var msg map[string]interface{}
	if json.Unmarshal(data, &msg) != nil {
		return
	}

	// For permission requests, the ID goes inside "data" (server expects it there).
	// For everything else (transcript, cancel), it goes at the top level.
	msgType, _ := msg["type"].(string)
	if msgType == "permission_request" {
		if dataField, ok := msg["data"].(map[string]interface{}); ok {
			dataField["ai_agent_instance_id"] = aw.aiAgentInstanceID
		}
	}
	msg["ai_agent_instance_id"] = aw.aiAgentInstanceID

	tagged, err := json.Marshal(msg)
	if err != nil {
		return
	}
	aw.daemon.ws.SendText(tagged)
}

// RegisterPending creates a channel for receiving a permission response.
func (aw *agentWS) RegisterPending(requestID string) <-chan []byte {
	return aw.daemon.ws.RegisterPending(requestID)
}

// RemovePending removes a pending request channel.
func (aw *agentWS) RemovePending(requestID string) {
	aw.daemon.ws.RemovePending(requestID)
}

// SendWSRequest sends an organization CRUD request to the server over the daemon
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
func (aw *agentWS) Send(data []byte) {}

// Close is a no-op — the shared connection is owned by the daemon.
func (aw *agentWS) Close() {}
