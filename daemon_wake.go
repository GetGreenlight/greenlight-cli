//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// sessionRecord is the persisted state of a completed session,
// containing everything needed to resume it.
type sessionRecord struct {
	ConversationID string `json:"conversation_id"`
	RelayID        string `json:"relay_id"`
	Agent          string `json:"agent"`
	Project        string `json:"project"`
	Cwd            string `json:"cwd"`
	DeviceID       string `json:"device_id"`
	EndedAt        string `json:"ended_at"`
}

// sessionStorePath returns ~/.greenlight/completed/
func sessionStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".greenlight", "completed")
}

// saveSessionRecord persists a session's state so it can be resumed later.
func saveSessionRecord(s *Session) {
	dir := sessionStorePath()
	if dir == "" {
		return
	}
	os.MkdirAll(dir, 0755)

	convID := lookupConversationID(s.relayID)
	if convID == "" {
		log.Printf("daemon: no conversation ID for relay %s, skipping session save", s.relayID)
		return
	}

	rec := sessionRecord{
		ConversationID: convID,
		RelayID:        s.relayID,
		Agent:          s.agent,
		Project:        s.project,
		Cwd:            s.cwd,
		DeviceID:       s.deviceID,
		EndedAt:        time.Now().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		log.Printf("daemon: failed to marshal session record: %v", err)
		return
	}

	path := filepath.Join(dir, convID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("daemon: failed to save session record: %v", err)
		return
	}
	log.Printf("daemon: saved session record %s", path)
}

// loadSessionRecord reads a persisted session record by conversation ID.
func loadSessionRecord(conversationID string) (*sessionRecord, error) {
	dir := sessionStorePath()
	if dir == "" {
		return nil, fmt.Errorf("cannot determine session store path")
	}
	path := filepath.Join(dir, conversationID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec sessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// loadSessionRecordByRelayID finds a session record by its relay ID.
func loadSessionRecordByRelayID(relayID string) (*sessionRecord, error) {
	records := listSessionRecords()
	for _, rec := range records {
		if rec.RelayID == relayID {
			return &rec, nil
		}
	}
	return nil, fmt.Errorf("no session record with relay_id %s", relayID)
}

// listSessionRecords returns all persisted session records, newest first.
func listSessionRecords() []sessionRecord {
	dir := sessionStorePath()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var records []sessionRecord
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec sessionRecord
		if json.Unmarshal(data, &rec) == nil {
			records = append(records, rec)
		}
	}
	return records
}

// removeSessionRecord deletes a persisted session record.
func removeSessionRecord(conversationID string) {
	dir := sessionStorePath()
	if dir == "" {
		return
	}
	os.Remove(filepath.Join(dir, conversationID+".json"))
}

// wakeSession resumes a dormant session by opening a new terminal window
// and running `greenlight connect --resume <conversationID>`.
// The connect command loads the session record itself, so we only need to
// pass --resume. It will auto-fill agent, device-id, project, and cd to
// the original working directory.
func wakeSession(rec *sessionRecord) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	connectCmd := fmt.Sprintf("cd %s && %s connect --resume %s",
		shellQuote(rec.Cwd),
		shellQuote(exePath),
		shellQuote(rec.ConversationID),
	)

	log.Printf("daemon: waking session %s: %s", rec.ConversationID, connectCmd)

	switch runtime.GOOS {
	case "darwin":
		return openTerminalDarwin(connectCmd)
	case "linux":
		return openTerminalLinux(connectCmd)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// openTerminalDarwin opens a new Terminal.app window and runs the command.
func openTerminalDarwin(cmd string) error {
	// Use osascript to tell Terminal.app to open a new window and run the command
	script := fmt.Sprintf(`tell application "Terminal"
	activate
	do script "%s"
end tell`, strings.ReplaceAll(cmd, `"`, `\"`))

	return exec.Command("osascript", "-e", script).Run()
}

// openTerminalLinux opens a new terminal emulator and runs the command.
func openTerminalLinux(cmd string) error {
	// Try common terminal emulators in order of preference
	terminals := []struct {
		bin  string
		args []string
	}{
		{"gnome-terminal", []string{"--", "bash", "-c", cmd + "; exec bash"}},
		{"xterm", []string{"-e", cmd}},
		{"konsole", []string{"-e", "bash", "-c", cmd + "; exec bash"}},
		{"xfce4-terminal", []string{"-e", cmd}},
	}

	for _, t := range terminals {
		if path, err := exec.LookPath(t.bin); err == nil {
			return exec.Command(path, t.args...).Start()
		}
	}

	return fmt.Errorf("no supported terminal emulator found")
}

// shellQuote wraps a string in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// handleWakeMessage processes a wake message from the server.
func (d *Daemon) handleWakeMessage(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("daemon: invalid wake message: %v", err)
		d.sendWakeResult("", false, "invalid wake message")
		return
	}
	if msg.RelayID == "" {
		log.Printf("daemon: wake message missing relay_id")
		d.sendWakeResult("", false, "missing relay_id")
		return
	}

	rec, err := loadSessionRecordByRelayID(msg.RelayID)
	if err != nil {
		log.Printf("daemon: wake: no session record for relay %s: %v", msg.RelayID, err)
		d.sendWakeResult(msg.RelayID, false, fmt.Sprintf("session not found: %v", err))
		return
	}

	if err := wakeSession(rec); err != nil {
		log.Printf("daemon: wake: failed to open terminal: %v", err)
		d.sendWakeResult(msg.RelayID, false, err.Error())
		return
	}

	log.Printf("daemon: woke session %s (relay %s)", rec.ConversationID, msg.RelayID)
	d.sendWakeResult(msg.RelayID, true, "")
}

// sendWakeResult sends a wake_result message back to the server.
func (d *Daemon) sendWakeResult(relayID string, success bool, errMsg string) {
	if d.daemonWS == nil {
		return
	}
	resp := map[string]interface{}{
		"type":     "wake_result",
		"relay_id": relayID,
		"success":  success,
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	d.daemonWS.ws.SendText(data)
}
