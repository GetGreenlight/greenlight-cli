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
	Ticket         string `json:"ticket,omitempty"`
	Cwd            string `json:"cwd"`
	Hostname       string `json:"hostname"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at"`
	Name           string `json:"name,omitempty"` // human-readable session title
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

	convID := s.convID
	if convID == "" {
		log.Printf("daemon: no conversation ID for relay %s, skipping session save", s.relayID)
		return
	}

	hostname, _ := os.Hostname()
	var name string
	if s.daemon != nil && s.daemon.daemonWS != nil {
		name = s.daemon.daemonWS.sessionName(s.relayID)
	}
	rec := sessionRecord{
		ConversationID: convID,
		RelayID:        s.relayID,
		Agent:          s.agent,
		Project:        s.project,
		Ticket:         s.ticket,
		Cwd:            s.cwd,
		Hostname:       hostname,
		StartedAt:      s.startedAt.Format(time.RFC3339),
		EndedAt:        time.Now().Format(time.RFC3339),
		Name:           name,
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

// updateSessionRecordName rewrites the on-disk record for a session, setting
// its name. Returns true if a record was found and updated. It is a no-op for
// live sessions, which have no record until they end.
func updateSessionRecordName(relayID, name string) bool {
	rec, err := loadSessionRecordByRelayID(relayID)
	if err != nil {
		return false
	}
	dir := sessionStorePath()
	if dir == "" {
		return false
	}
	rec.Name = name
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return false
	}
	path := filepath.Join(dir, rec.ConversationID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("daemon: failed to update session record name: %v", err)
		return false
	}
	return true
}

// wakeSession resumes a dormant session by opening a new terminal window
// and running `greenlight connect --resume <conversationID>`.
// The connect command loads the session record itself, so we only need to
// pass --resume. It will auto-fill agent, device-id, project, and cd to
// the original working directory.
func wakeSession(rec *sessionRecord, deviceID string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	// Clear any inherited greenlight env vars before launching connect. The
	// wake terminal descends from whichever daemon first launched Terminal.app
	// (or the terminal emulator on Linux), so it can carry a stale snapshot of
	// GREENLIGHT_DEVICE_ID / GREENLIGHT_DAEMON_SESSION_ID from an earlier daemon
	// that has since been restarted with a different device ID. Pin the device
	// ID explicitly via --device-id so the resumed session always matches the
	// daemon that woke it, regardless of what the inherited env says.
	deviceFlag := ""
	if deviceID != "" {
		deviceFlag = "--device-id " + shellQuote(deviceID) + " "
	}
	connectCmd := fmt.Sprintf("unset GREENLIGHT_DEVICE_ID GREENLIGHT_DAEMON_SESSION_ID; cd %s && %s connect %s--resume %s",
		shellQuote(rec.Cwd),
		shellQuote(exePath),
		deviceFlag,
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

// resolveSessionEnv discovers the current graphical session's environment
// on Linux. The daemon is a long-running background process whose inherited
// DISPLAY, WAYLAND_DISPLAY, and DBUS_SESSION_BUS_ADDRESS may be stale
// (e.g. if the user logged out and back in). We query systemd --user for
// the live values so terminal emulators can connect to the display server.
func resolveSessionEnv() []string {
	uid := os.Getuid()
	dbusAddr := fmt.Sprintf("unix:path=/run/user/%d/bus", uid)

	cmd := exec.Command("systemctl", "--user", "show-environment")
	cmd.Env = []string{
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
		"DBUS_SESSION_BUS_ADDRESS=" + dbusAddr,
	}
	out, err := cmd.Output()
	if err != nil {
		log.Printf("daemon: systemctl --user show-environment failed: %v", err)
		return nil
	}

	// Extract display and session vars needed by terminal emulators.
	var env []string
	for _, line := range strings.Split(string(out), "\n") {
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS",
			"XDG_RUNTIME_DIR", "XDG_SESSION_TYPE", "XAUTHORITY":
			env = append(env, line)
		}
	}
	if len(env) > 0 {
		log.Printf("daemon: resolved session env: %v", env)
	}
	return env
}

// openTerminalLinux opens a new terminal emulator and runs the command.
// Tries each available terminal in order, falling through on failure.
// Uses Run() with a short timeout to detect quick failures (e.g. D-Bus
// errors from gnome-terminal when launched from a background daemon).
func openTerminalLinux(cmd string) error {
	shellCmd := cmd + "; exec bash"
	// All entries use "bash -c" explicitly so the shell command is
	// interpreted correctly regardless of how each terminal handles -e.
	terminals := []struct {
		bin  string
		args []string
	}{
		{"gnome-terminal", []string{"--", "bash", "-c", shellCmd}},
		{"x-terminal-emulator", []string{"-e", "bash", "-c", shellCmd}},
		{"konsole", []string{"-e", "bash", "-c", shellCmd}},
		{"xfce4-terminal", []string{"-x", "bash", "-c", shellCmd}},
		{"alacritty", []string{"-e", "bash", "-c", shellCmd}},
		{"kitty", []string{"bash", "-c", shellCmd}},
		{"xterm", []string{"-e", "bash", "-c", shellCmd}},
	}

	// Discover the current graphical session environment so terminal
	// emulators can reach the display server and D-Bus, even if the
	// daemon's inherited env is stale.
	sessionEnv := resolveSessionEnv()
	var termEnv []string
	if len(sessionEnv) > 0 {
		termEnv = append(os.Environ(), sessionEnv...)
	}

	var lastErr error
	for _, t := range terminals {
		path, err := exec.LookPath(t.bin)
		if err != nil {
			continue
		}

		c := exec.Command(path, t.args...)
		if len(termEnv) > 0 {
			c.Env = termEnv
		}
		if err := c.Start(); err != nil {
			lastErr = fmt.Errorf("%s: start: %w", t.bin, err)
			log.Printf("daemon: terminal %s failed to start: %v", t.bin, err)
			continue
		}

		// Wait briefly for the process to exit. Terminals like
		// gnome-terminal use a client-server model where the client
		// exits quickly — a non-zero exit code means it couldn't
		// open a window (e.g. D-Bus unreachable). Terminals like
		// xterm run for the lifetime of the window, so if the
		// process is still alive after 3s we assume it worked.
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()

		select {
		case err := <-done:
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", t.bin, err)
				log.Printf("daemon: terminal %s exited with error: %v", t.bin, err)
				continue // try next terminal
			}
			// Exited successfully (client-server model)
			log.Printf("daemon: opened terminal via %s", t.bin)
			return nil
		case <-time.After(3 * time.Second):
			// Still running — terminal window is up
			log.Printf("daemon: opened terminal via %s (still running)", t.bin)
			return nil
		}
	}

	if lastErr != nil {
		return fmt.Errorf("all terminals failed, last: %w", lastErr)
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

	if err := wakeSession(rec, d.deviceID); err != nil {
		log.Printf("daemon: wake: failed to open terminal: %v", err)
		d.sendWakeResult(msg.RelayID, false, err.Error())
		return
	}

	log.Printf("daemon: woke session %s (relay %s)", rec.ConversationID, msg.RelayID)
	d.sendWakeResult(msg.RelayID, true, "")
}

// handleNewSessionMessage processes a new_session message from the server. It
// spawns a new terminal window running `greenlight connect` with the requested
// agent and cwd. Unlike wake, there's no existing relay_id — the spawned
// session will register itself when it enrolls.
func (d *Daemon) handleNewSessionMessage(data []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Cwd       string `json:"cwd"`
		Agent     string `json:"agent"`
		Ticket    string `json:"ticket"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("daemon: invalid new_session message: %v", err)
		d.sendNewSessionResult("", false, "invalid new_session message")
		return
	}
	if msg.Cwd == "" {
		d.sendNewSessionResult(msg.RequestID, false, "missing cwd")
		return
	}
	if info, err := os.Stat(msg.Cwd); err != nil || !info.IsDir() {
		d.sendNewSessionResult(msg.RequestID, false, fmt.Sprintf("cwd not a directory: %s", msg.Cwd))
		return
	}
	if msg.Agent != "" && !knownAgents[msg.Agent] {
		d.sendNewSessionResult(msg.RequestID, false, fmt.Sprintf("unknown agent: %s", msg.Agent))
		return
	}

	if err := newSession(msg.Cwd, msg.Agent, msg.Ticket, d.deviceID); err != nil {
		log.Printf("daemon: new_session: failed to open terminal: %v", err)
		d.sendNewSessionResult(msg.RequestID, false, err.Error())
		return
	}

	log.Printf("daemon: spawned new session in %s (agent %q)", msg.Cwd, msg.Agent)
	d.sendNewSessionResult(msg.RequestID, true, "")
}

// newSession spawns a new terminal window running `greenlight connect` with
// the given agent and cwd. If agent is empty, connect's normal resolution
// (env > config > default) applies.
func newSession(cwd, agent, ticket, deviceID string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	deviceFlag := ""
	if deviceID != "" {
		deviceFlag = "--device-id " + shellQuote(deviceID) + " "
	}
	agentFlag := ""
	if agent != "" {
		agentFlag = "--agent " + shellQuote(agent) + " "
	}
	ticketFlag := ""
	if ticket != "" {
		ticketFlag = "--ticket " + shellQuote(ticket) + " "
	}
	connectCmd := fmt.Sprintf("unset GREENLIGHT_DEVICE_ID GREENLIGHT_DAEMON_SESSION_ID; cd %s && %s connect %s%s%s",
		shellQuote(cwd),
		shellQuote(exePath),
		deviceFlag,
		agentFlag,
		ticketFlag,
	)
	connectCmd = strings.TrimRight(connectCmd, " ")

	log.Printf("daemon: spawning new session: %s", connectCmd)

	switch runtime.GOOS {
	case "darwin":
		return openTerminalDarwin(connectCmd)
	case "linux":
		return openTerminalLinux(connectCmd)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// sendNewSessionResult sends a new_session_result message back to the server.
func (d *Daemon) sendNewSessionResult(requestID string, success bool, errMsg string) {
	if d.daemonWS == nil {
		return
	}
	resp := map[string]interface{}{
		"type":       "new_session_result",
		"request_id": requestID,
		"success":    success,
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
