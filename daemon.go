//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// IPC frame types for binary-framed PTY I/O between client and daemon.
const (
	frameStdin         byte = 0x01
	frameStdout        byte = 0x02
	frameResize        byte = 0x03
	frameSignal        byte = 0x04
	frameExit          byte = 0x05
	framePrompt        byte = 0x06
	framePromptResp    byte = 0x07
	framePromptCancel  byte = 0x08
)

// ipcRequest is the JSON control message sent by the client to the daemon.
type ipcRequest struct {
	Type     string            `json:"type"`               // connect, status, stop, update_shutdown
	Agent    string            `json:"agent,omitempty"`
	DeviceID string            `json:"device_id,omitempty"`
	Project  string            `json:"project,omitempty"`
	Resume   string            `json:"resume,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Winsize  *ipcWinsize       `json:"winsize,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Force    bool              `json:"force,omitempty"`
}

// ipcResponse is the JSON control message sent by the daemon to the client.
type ipcResponse struct {
	Type      string          `json:"type"`                 // session_started, error, status_response, ok
	SessionID string          `json:"session_id,omitempty"`
	RelayID   string          `json:"relay_id,omitempty"`
	Message   string          `json:"message,omitempty"`
	Sessions  []sessionInfo   `json:"sessions,omitempty"`
	History   []sessionRecord `json:"history,omitempty"`
}

type ipcWinsize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type sessionInfo struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Project   string `json:"project"`
	Cwd       string `json:"cwd"`
	StartedAt string `json:"started_at"`
	Resumable bool   `json:"resumable,omitempty"`
}

// Daemon manages the lifecycle of agent sessions and the IPC socket.
type Daemon struct {
	listener  net.Listener
	sockPath  string
	sessions  map[string]*Session
	mu        sync.RWMutex
	done      chan struct{}
	wg        sync.WaitGroup
	daemonWS  *DaemonWS // multiplexed WebSocket for all sessions + wake
	sessionID string    // daemon's own enrolled session ID
	deviceID  string    // resolved device ID
}

func runDaemon(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: greenlight daemon <start|stop|status>\n")
		os.Exit(1)
	}

	switch args[0] {
	case "start":
		foreground := false
		var deviceIDFlag string
		for i, a := range args[1:] {
			if a == "--foreground" {
				foreground = true
			}
			if a == "--device-id" && i+1 < len(args[1:]) {
				deviceIDFlag = args[1:][i+1]
			}
		}
		if foreground {
			runDaemonForeground()
		} else {
			if isDaemonRunning() {
				fmt.Fprintf(os.Stderr, "greenlight: daemon is already running\n")
				return
			}
			if err := ensureDaemon(deviceIDFlag); err != nil {
				fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
				os.Exit(1)
			}
		}
	case "stop":
		stopDaemon()
	case "status":
		daemonStatus()
	default:
		fmt.Fprintf(os.Stderr, "greenlight daemon: unknown subcommand %q\n", args[0])
		os.Exit(1)
	}
}

// daemonSockPath returns the path to the daemon's Unix socket.
// Uses /tmp to keep the path short (Unix sockets limited to ~104 bytes on macOS).
// Can be overridden with GREENLIGHT_DAEMON_SOCK for testing.
func daemonSockPath() string {
	if p := os.Getenv("GREENLIGHT_DAEMON_SOCK"); p != "" {
		return p
	}
	return "/tmp/greenlight-daemon.sock"
}

// daemonPidPath returns the path to the daemon's PID file.
func daemonPidPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/greenlight-daemon.pid"
	}
	return filepath.Join(home, ".greenlight", "daemon.pid")
}

// isDaemonRunning checks if a daemon is already running by probing the socket.
func isDaemonRunning() bool {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// startDaemonBackground forks the daemon as a background process.
func startDaemonBackground() {
	if isDaemonRunning() {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is already running\n")
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: cannot resolve executable: %v\n", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	cmd := exec.Command(exePath, "daemon", "start", "--foreground")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to start daemon: %v\n", err)
		os.Exit(1)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()

	// Wait for the socket to become available
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if isDaemonRunning() {
			fmt.Fprintf(os.Stderr, "greenlight: daemon started (pid %d)\n", pid)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "greenlight: daemon started but socket not ready\n")
}

// ensureDaemon starts the daemon if it's not already running.
// Returns an error if the daemon cannot be started.
func ensureDaemon(deviceIDFlag string) error {
	if isDaemonRunning() {
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	// Enroll the daemon before starting it (foreground, so the user sees progress)
	var sessionID, deviceID string
	if wsURL != "" {
		// Resolve device ID: flag > env > config (same priority as connect)
		deviceID = deviceIDFlag
		if deviceID == "" {
			deviceID = os.Getenv("GREENLIGHT_DEVICE_ID")
		}
		if deviceID == "" {
			deviceID = readConfigValue("device_id")
		}
		if deviceID != "" {
			sessionID = generateUUID()
			baseURL, err := serverBaseURL()
			if err != nil {
				return fmt.Errorf("cannot derive server URL: %w", err)
			}
			hostname, _ := os.Hostname()
			fmt.Fprintf(os.Stderr, "greenlight: enrolling daemon (approve on your phone)...\n")
			if err := enrollSession(baseURL, deviceID, sessionID, "", "", "", hostname); err != nil {
				return fmt.Errorf("daemon enrollment failed: %w", err)
			}
		}
	}

	cmd := exec.Command(exePath, "daemon", "start", "--foreground")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Pass enrolled session and device ID to the daemon via env
	if sessionID != "" {
		cmd.Env = append(os.Environ(),
			"GREENLIGHT_DAEMON_SESSION_ID="+sessionID,
			"GREENLIGHT_DEVICE_ID="+deviceID,
		)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	cmd.Process.Release()

	// Wait for the socket to become available
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if isDaemonRunning() {
			return nil
		}
	}
	return fmt.Errorf("daemon started but socket not ready")
}

// runDaemonForeground runs the daemon in the foreground (used by background start).
func runDaemonForeground() {
	// Set up logging
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".greenlight")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "daemon.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		log.SetOutput(f)
	}

	sockPath := daemonSockPath()

	// Ensure parent directory exists
	os.MkdirAll(filepath.Dir(sockPath), 0755)

	// Remove stale socket
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Printf("daemon: failed to listen on %s: %v", sockPath, err)
		fmt.Fprintf(os.Stderr, "greenlight: failed to listen on %s: %v\n", sockPath, err)
		os.Exit(1)
	}

	d := &Daemon{
		listener: listener,
		sockPath: sockPath,
		sessions: make(map[string]*Session),
		done:     make(chan struct{}),
	}

	// Write PID file
	pidPath := daemonPidPath()
	os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(pidPath)

	log.Printf("daemon: started (pid %d, socket %s)", os.Getpid(), sockPath)

	// Resolve device ID and session ID (set by ensureDaemon after enrollment)
	d.deviceID = os.Getenv("GREENLIGHT_DEVICE_ID")
	if d.deviceID == "" {
		d.deviceID = readConfigValue("device_id")
	}
	d.sessionID = os.Getenv("GREENLIGHT_DAEMON_SESSION_ID")
	if d.sessionID != "" {
		log.Printf("daemon: using pre-enrolled session %s", d.sessionID)
	}

	// Start the multiplexed WebSocket — all sessions share this connection
	// for transcript, permissions, and control messages (wake, kill).
	d.startDaemonWS()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("daemon: shutting down")
		d.Shutdown()
	}()

	d.Run()

	log.Printf("daemon: stopped")
}

// Run accepts connections and handles them until the daemon is shut down.
func (d *Daemon) Run() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.done:
				d.wg.Wait()
				return
			default:
				log.Printf("daemon: accept error: %v", err)
				continue
			}
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleConn(conn)
		}()
	}
}

// Shutdown gracefully stops the daemon, terminating all sessions.
func (d *Daemon) Shutdown() {
	close(d.done)
	d.listener.Close()
	os.Remove(d.sockPath)

	// Close multiplexed WebSocket
	if d.daemonWS != nil {
		d.daemonWS.Close()
	}

	// Stop all sessions
	d.mu.Lock()
	for id, s := range d.sessions {
		log.Printf("daemon: stopping session %s", id)
		s.Stop()
	}
	d.mu.Unlock()
}

// handleUpdateShutdown handles the update_shutdown IPC request.
// If there are active sessions and force is false, it reports them back.
// Otherwise it saves session records and shuts down.
func (d *Daemon) handleUpdateShutdown(conn net.Conn, req ipcRequest) {
	d.mu.RLock()
	count := len(d.sessions)
	d.mu.RUnlock()

	if count > 0 && !req.Force {
		// Report active sessions so the update command can prompt the user
		d.mu.RLock()
		var sessions []sessionInfo
		for _, s := range d.sessions {
			resumable := agentSupportsResume(s.agent) && lookupConversationID(s.relayID) != ""
			sessions = append(sessions, sessionInfo{
				SessionID: s.relayID,
				Agent:     s.agent,
				Project:   s.project,
				Cwd:       s.cwd,
				StartedAt: s.startedAt.Format(time.RFC3339),
				Resumable: resumable,
			})
		}
		d.mu.RUnlock()
		sendControl(conn, ipcResponse{Type: "active_sessions", Sessions: sessions})
		return
	}

	sendControl(conn, ipcResponse{Type: "ok"})
	go d.ShutdownForUpdate()
}

// ShutdownForUpdate saves session records before stopping sessions,
// so they can be resumed after the update.
func (d *Daemon) ShutdownForUpdate() {
	// Save session records before killing sessions
	d.mu.RLock()
	for _, s := range d.sessions {
		saveSessionRecord(s)
	}
	d.mu.RUnlock()

	d.Shutdown()
}

// startDaemonWS starts the multiplexed WebSocket connection. All sessions
// share this single connection for transcript, permissions, and control messages.
// The connection is maintained with auto-reconnect for the lifetime of the daemon.
func (d *Daemon) startDaemonWS() {
	if wsURL == "" {
		log.Printf("daemon: no relay URL configured, WebSocket disabled")
		return
	}

	if d.deviceID == "" {
		log.Printf("daemon: no device ID configured, WebSocket disabled")
		return
	}

	if d.sessionID == "" {
		log.Printf("daemon: no enrolled session, WebSocket disabled")
		return
	}

	// Build the daemon WebSocket URL (/ws/daemon instead of /ws/relay)
	dialURL, err := url.Parse(wsURL)
	if err != nil {
		log.Printf("daemon: bad relay URL: %v", err)
		return
	}
	// Replace /ws/relay path with /ws/daemon
	dialURL.Path = strings.TrimSuffix(dialURL.Path, "/relay") + "/daemon"
	q := dialURL.Query()
	q.Set("session_id", d.sessionID)
	if version != "" {
		q.Set("version", version)
	}
	dialURL.RawQuery = q.Encode()

	d.daemonWS = NewDaemonWS(dialURL.String(), d.deviceID)
	d.daemonWS.SetWakeHandler(func(data []byte) {
		d.handleWakeMessage(data)
	})

	go d.daemonWS.Run()
	log.Printf("daemon: WebSocket started (device %s, session %s)", d.deviceID, d.sessionID)
}

// handleConn processes a single client connection.
func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()

	// Read control message (JSON, newline-delimited)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		log.Printf("daemon: read control message error: %v", err)
		return
	}
	conn.SetReadDeadline(time.Time{}) // clear deadline for session I/O

	var req ipcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("daemon: invalid control message: %v", err)
		sendControl(conn, ipcResponse{Type: "error", Message: "invalid request"})
		return
	}

	switch req.Type {
	case "connect":
		d.handleConnect(conn, req)
	case "status":
		d.handleStatus(conn)
	case "stop":
		sendControl(conn, ipcResponse{Type: "ok"})
		go d.Shutdown()
	case "update_shutdown":
		d.handleUpdateShutdown(conn, req)
	case "session_history":
		sendControl(conn, ipcResponse{Type: "session_history_response", History: listSessionRecords()})
	default:
		sendControl(conn, ipcResponse{Type: "error", Message: "unknown request type"})
	}
}

// handleStatus sends session information back to the client.
func (d *Daemon) handleStatus(conn net.Conn) {
	d.mu.RLock()
	var sessions []sessionInfo
	for _, s := range d.sessions {
		sessions = append(sessions, sessionInfo{
			SessionID: s.relayID,
			Agent:     s.agent,
			Project:   s.project,
			Cwd:       s.cwd,
			StartedAt: s.startedAt.Format(time.RFC3339),
		})
	}
	d.mu.RUnlock()

	sendControl(conn, ipcResponse{
		Type:     "status_response",
		Sessions: sessions,
	})
}

// handleConnect creates a new session and enters the I/O relay phase.
func (d *Daemon) handleConnect(conn net.Conn, req ipcRequest) {
	// Verify device ID matches the daemon's enrolled device if provided
	if req.DeviceID != "" && d.deviceID != "" && req.DeviceID != d.deviceID {
		sendControl(conn, ipcResponse{
			Type:    "error",
			Message: fmt.Sprintf("device ID mismatch: daemon enrolled as %s, got %s", d.deviceID, req.DeviceID),
		})
		return
	}

	s, err := d.newSession(req)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}

	d.mu.Lock()
	d.sessions[s.relayID] = s
	d.mu.Unlock()

	// Send success response to client
	sendControl(conn, ipcResponse{
		Type:      "session_started",
		SessionID: s.relayID,
		RelayID:   s.relayID,
	})

	// Enter binary I/O relay phase — blocks until session ends or client disconnects
	s.AttachClient(conn)

	// Clean up session after client disconnects
	d.mu.Lock()
	delete(d.sessions, s.relayID)
	d.mu.Unlock()

	s.Stop()
	log.Printf("daemon: session %s ended", s.relayID)
}

// sendControl writes a JSON control message followed by a newline.
func sendControl(conn net.Conn, resp ipcResponse) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	conn.Write(data)
}

// writeFrame writes a binary IPC frame: [type:1][length:4][payload].
func writeFrame(w io.Writer, frameType byte, payload []byte) error {
	header := [5]byte{frameType}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// readFrame reads a binary IPC frame. Returns type and payload.
func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	frameType := header[0]
	length := binary.BigEndian.Uint32(header[1:])
	if length > 1<<20 { // 1MB sanity limit
		return 0, nil, fmt.Errorf("frame too large: %d bytes", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return frameType, payload, nil
}

func stopDaemon() {
	// Check for active sessions first
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is not running\n")
		return
	}

	// Query status to check for active sessions
	statusMsg, _ := json.Marshal(ipcRequest{Type: "status"})
	statusMsg = append(statusMsg, '\n')
	conn.Write(statusMsg)

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	conn.Close()

	if err == nil {
		var status ipcResponse
		if json.Unmarshal(line, &status) == nil && len(status.Sessions) > 0 {
			fmt.Fprintf(os.Stderr, "greenlight: %d active session(s) will be terminated:\n", len(status.Sessions))
			for _, s := range status.Sessions {
				fmt.Fprintf(os.Stderr, "  %-10s %-8s %s\n",
					s.SessionID[:min(10, len(s.SessionID))], s.Agent, s.Project)
			}
			fmt.Fprintf(os.Stderr, "Continue? [y/N] ")
			var answer string
			fmt.Scanln(&answer)
			if answer != "y" && answer != "Y" {
				fmt.Fprintf(os.Stderr, "greenlight: stop cancelled\n")
				return
			}
		}
	}

	// Send stop
	conn, err = net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is not running\n")
		return
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "stop"})
	msg = append(msg, '\n')
	conn.Write(msg)

	reader = bufio.NewReader(conn)
	line, err = reader.ReadBytes('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: daemon stopped\n")
		return
	}
	var resp ipcResponse
	json.Unmarshal(line, &resp)
	if resp.Type == "ok" {
		fmt.Fprintf(os.Stderr, "greenlight: daemon stopped\n")
	} else {
		fmt.Fprintf(os.Stderr, "greenlight: %s\n", resp.Message)
	}
}

func daemonStatus() {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is not running\n")
		return
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "status"})
	msg = append(msg, '\n')
	conn.Write(msg)

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to read response\n")
		return
	}
	var resp ipcResponse
	json.Unmarshal(line, &resp)

	// Read PID
	pidData, _ := os.ReadFile(daemonPidPath())
	pid := strings.TrimSpace(string(pidData))

	if len(resp.Sessions) == 0 {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is running (pid %s), no active sessions\n", pid)
	} else {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is running (pid %s), %d active session(s):\n", pid, len(resp.Sessions))
		for _, s := range resp.Sessions {
			fmt.Fprintf(os.Stderr, "  %s  %s  %s  %s\n", s.SessionID[:8], s.Agent, s.Project, s.Cwd)
		}
	}
}
