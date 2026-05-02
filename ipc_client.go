//go:build darwin || linux

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// ipcDial opens a connection to the running daemon's IPC socket.
func ipcDial() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon not running (start it with `greenlight daemon start`): %w", err)
	}
	return conn, nil
}

// ipcExchange sends one IPC request and reads one response line.
func ipcExchange(req ipcRequest) (*ipcResponse, error) {
	conn, err := ipcDial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(body); err != nil {
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})

	// Long-poll-friendly read deadline.
	conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("invalid daemon response: %w", err)
	}
	if resp.Type == "error" {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	return &resp, nil
}

// daemonWSRequest forwards a daemon-WS message via the running daemon and
// returns the raw response payload (full JSON message echoed by the server).
func daemonWSRequest(wsType string, payload interface{}, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	rid, err := newRequestID()
	if err != nil {
		return nil, err
	}

	// Inject request_id into the payload so the server can echo it back.
	var payloadMap map[string]interface{}
	if payload == nil {
		payloadMap = map[string]interface{}{}
	} else {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(buf, &payloadMap); err != nil {
			return nil, err
		}
	}
	payloadMap["request_id"] = rid

	dataBytes, err := json.Marshal(payloadMap)
	if err != nil {
		return nil, err
	}

	req := ipcRequest{
		Type:      "ws_request",
		WSType:    wsType,
		WSData:    dataBytes,
		WSReqID:   rid,
		TimeoutMS: int(timeout / time.Millisecond),
	}
	resp, err := ipcExchange(req)
	if err != nil {
		return nil, err
	}
	if resp.Type != "ws_response" {
		return nil, fmt.Errorf("unexpected ipc response type %q", resp.Type)
	}
	return resp.WSResponse, nil
}

// newRequestID returns a random hex string (16 bytes / 32 chars).
func newRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
