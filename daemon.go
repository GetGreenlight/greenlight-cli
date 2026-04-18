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
	frameStdin        byte = 0x01
	frameStdout       byte = 0x02
	frameResize       byte = 0x03
	frameSignal       byte = 0x04
	frameExit         byte = 0x05
	framePrompt       byte = 0x06
	framePromptResp   byte = 0x07
	framePromptCancel byte = 0x08
)

// ipcRequest is the JSON control message sent by the client to the daemon.
type ipcRequest struct {
	Type      string            `json:"type"` // connect, status, stop, update_shutdown, ws_request
	Agent     string            `json:"agent,omitempty"`
	DeviceID  string            `json:"device_id,omitempty"`
	Project   string            `json:"project,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Winsize   *ipcWinsize       `json:"winsize,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Force     bool              `json:"force,omitempty"`
	WSMsgType string            `json:"ws_msg_type,omitempty"` // for ws_request: the CRUD message type
	WSData    json.RawMessage   `json:"ws_data,omitempty"`     // for ws_request: the payload
	// Detached is set by the in-process spawnAgentInstance path used by
	// 'org agent create'. It tells newAgentInstance to skip prerequisites that
	// only make sense for client-attached connect sessions.
	Detached bool `json:"-"`
	// AIAgentInstanceID is an optional override used when the caller already
	// has a stable identifier it wants to use as the instance's ID (e.g. the
	// row id from create_ai_agent_instance). When empty, buildAgentCommand
	// generates one.
	AIAgentInstanceID string `json:"-"`
}

// ipcResponse is the JSON control message sent by the daemon to the client.
type ipcResponse struct {
	Type              string          `json:"type"` // agent_instance_started, error, status_response, ok, ws_response
	AIAgentInstanceID string          `json:"ai_agent_instance_id,omitempty"`
	Message           string          `json:"message,omitempty"`
	Version           string          `json:"version,omitempty"`
	Instances         []instanceInfo  `json:"instances,omitempty"`
	Data              json.RawMessage `json:"data,omitempty"` // for ws_response: the server response payload
}

type ipcWinsize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type instanceInfo struct {
	AIAgentInstanceID string `json:"ai_agent_instance_id"`
	Agent             string `json:"agent"`
	Project           string `json:"project"`
	Cwd               string `json:"cwd"`
	StartedAt         string `json:"started_at"`
}

// Daemon manages the lifecycle of agent instances and the IPC socket.
type Daemon struct {
	listener    net.Listener
	sockPath    string
	instances   map[string]*AgentInstance
	mu          sync.RWMutex
	done        chan struct{}
	wg          sync.WaitGroup
	daemonWS    *DaemonWS // multiplexed WebSocket for all instances
	hostID      string    // daemon's registered host ID
	humanUserID string    // resolved human_user ID
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

	// Resolve device ID and host ID, registering the host if this is the
	// first run on this machine.
	var hostID, deviceID string
	if wsURL != "" {
		// Resolve device ID: flag > env > config (same priority as connect)
		deviceID = deviceIDFlag
		if deviceID == "" {
			deviceID = os.Getenv("GREENLIGHT_DEVICE_ID")
		}
		if deviceID == "" {
			deviceID = readConfigValue("device_id")
		}

		// Use persisted host_id if available; otherwise generate and register
		hostID = readConfigValue("host_id")
		if hostID == "" {
			hostID = generateUUID()
			baseURL, err := serverBaseURL()
			if err != nil {
				return fmt.Errorf("cannot derive server URL: %w", err)
			}
			hostname, _ := os.Hostname()

			userID := readConfigValue("user_id")
			if userID == "" {
				return fmt.Errorf("not registered — run 'greenlight register <email>' first")
			}
			ioDeviceID, err := registerHost(baseURL, userID, hostID, hostname)
			if err != nil {
				return fmt.Errorf("host registration failed: %w", err)
			}
			if ioDeviceID != "" {
				if err := writeConfigValue("io_device_id", ioDeviceID); err != nil {
					return fmt.Errorf("failed to persist io_device_id: %w", err)
				}
			}
			if err := writeConfigValue("host_id", hostID); err != nil {
				return fmt.Errorf("failed to persist host_id: %w", err)
			}
		}
	}

	cmd := exec.Command(exePath, "daemon", "start", "--foreground")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Pass host ID and device ID to the daemon via env
	if hostID != "" {
		cmd.Env = append(os.Environ(),
			"GREENLIGHT_DAEMON_HOST_ID="+hostID,
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
		listener:  listener,
		sockPath:  sockPath,
		instances: make(map[string]*AgentInstance),
		done:      make(chan struct{}),
	}

	// Write PID file
	pidPath := daemonPidPath()
	os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(pidPath)

	log.Printf("daemon: started (pid %d, socket %s)", os.Getpid(), sockPath)

	// Resolve human_user ID and host ID (set by ensureDaemon after registration, or from config)
	d.humanUserID = readConfigValue("user_id")
	d.hostID = os.Getenv("GREENLIGHT_DAEMON_HOST_ID")
	if d.hostID == "" && d.humanUserID != "" {
		d.hostID = readConfigValue("host_id")
	}
	if d.hostID != "" {
		log.Printf("daemon: using host %s", d.hostID)
	}

	// Start the multiplexed WebSocket — all instances share this connection
	// for transcript, permissions, and control messages.
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

// Shutdown gracefully stops the daemon, terminating all instances.
func (d *Daemon) Shutdown() {
	close(d.done)
	d.listener.Close()
	os.Remove(d.sockPath)

	// Close multiplexed WebSocket
	if d.daemonWS != nil {
		d.daemonWS.Close()
	}

	// Stop all instances (this includes detached spawn instances from
	// 'org agent create' since they live in d.instances too).
	d.mu.Lock()
	for id, s := range d.instances {
		log.Printf("daemon: stopping agent instance %s", id)
		s.Stop()
	}
	d.mu.Unlock()
}

// handleUpdateShutdown handles the update_shutdown IPC request.
// If there are active instances and force is false, it reports them back.
// Otherwise it shuts down.
func (d *Daemon) handleUpdateShutdown(conn net.Conn, req ipcRequest) {
	d.mu.RLock()
	count := len(d.instances)
	d.mu.RUnlock()

	if count > 0 && !req.Force {
		// Report active instances so the update command can prompt the user
		d.mu.RLock()
		var instances []instanceInfo
		for _, s := range d.instances {
			instances = append(instances, instanceInfo{
				AIAgentInstanceID: s.aiAgentInstanceID,
				Agent:             s.agent,
				Project:           s.project,
				Cwd:               s.cwd,
				StartedAt:         s.startedAt.Format(time.RFC3339),
			})
		}
		d.mu.RUnlock()
		sendControl(conn, ipcResponse{Type: "active_instances", Instances: instances})
		return
	}

	sendControl(conn, ipcResponse{Type: "ok"})
	go d.Shutdown()
}

// startDaemonWS starts the multiplexed WebSocket connection. All instances
// share this single connection for transcript, permissions, and control messages.
// The connection is maintained with auto-reconnect for the lifetime of the daemon.
func (d *Daemon) startDaemonWS() {
	if wsURL == "" {
		log.Printf("daemon: no relay URL configured, WebSocket disabled")
		return
	}

	if d.humanUserID == "" {
		log.Printf("daemon: no human_user ID configured, WebSocket disabled")
		return
	}

	if d.hostID == "" {
		log.Printf("daemon: no host ID configured, WebSocket disabled")
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
	q.Set("host_id", d.hostID)
	if hostname, err := os.Hostname(); err == nil {
		q.Set("hostname", hostname)
	}
	if version != "" {
		q.Set("version", version)
	}
	dialURL.RawQuery = q.Encode()

	d.daemonWS = NewDaemonWS(dialURL.String(), d.humanUserID)

	go d.daemonWS.Run()

	// Wait for the WebSocket to connect before accepting IPC connections,
	// so the first agent_instance_connect message is sent over a live
	// connection rather than queued and drained later.
	for i := 0; i < 100; i++ {
		if d.daemonWS.IsConnected() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !d.daemonWS.IsConnected() {
		log.Printf("daemon: WARNING WebSocket not connected after 5s, proceeding anyway")
	}
	log.Printf("daemon: WebSocket started (human_user %s, host %s)", d.humanUserID, d.hostID)
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
	case "ws_request":
		d.handleWSRequest(conn, req)
	default:
		sendControl(conn, ipcResponse{Type: "error", Message: "unknown request type"})
	}
}

// handleWSRequest forwards a CRUD message to the server over the daemon WebSocket
// and returns the response to the IPC client.
func (d *Daemon) handleWSRequest(conn net.Conn, req ipcRequest) {
	if d.daemonWS == nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon WebSocket not available"})
		return
	}
	if !d.daemonWS.IsConnected() {
		sendControl(conn, ipcResponse{Type: "error", Message: "daemon WebSocket not connected"})
		return
	}

	// create_ai_agent_instance gets special treatment: the daemon pre-checks
	// host locality, forwards the create, then spawns the agent process.
	if req.WSMsgType == "create_ai_agent_instance" {
		d.handleCreateAgentInstance(conn, req)
		return
	}

	// update_ai_agent_instance with a non-empty retired_at is the "stop"
	// path: terminate the local instance (if any) before forwarding the row
	// update. Instances from spawnAgentInstance are keyed by
	// ai_agent_instance_id in d.instances, so the lookup is direct.
	if req.WSMsgType == "update_ai_agent_instance" {
		var probe struct {
			ID        string `json:"id"`
			RetiredAt string `json:"retired_at"`
		}
		if json.Unmarshal(req.WSData, &probe) == nil && probe.RetiredAt != "" && probe.ID != "" {
			d.mu.Lock()
			s := d.instances[probe.ID]
			delete(d.instances, probe.ID)
			d.mu.Unlock()
			if s != nil {
				log.Printf("daemon: stopping spawned agent instance %s on retire", probe.ID)
				s.Stop()
			}
		}
	}

	correlationID := generateUUID()
	resp, err := d.daemonWS.SendWSRequest(correlationID, req.WSMsgType, req.WSData)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "ws_response", Data: json.RawMessage(resp)})
}

// handleStatus sends instance information back to the client.
func (d *Daemon) handleStatus(conn net.Conn) {
	d.mu.RLock()
	var instances []instanceInfo
	for _, s := range d.instances {
		instances = append(instances, instanceInfo{
			AIAgentInstanceID: s.aiAgentInstanceID,
			Agent:             s.agent,
			Project:           s.project,
			Cwd:               s.cwd,
			StartedAt:         s.startedAt.Format(time.RFC3339),
		})
	}
	d.mu.RUnlock()

	sendControl(conn, ipcResponse{
		Type:      "status_response",
		Instances: instances,
	})
}

// handleConnect creates a new agent instance and enters the I/O relay phase.
func (d *Daemon) handleConnect(conn net.Conn, req ipcRequest) {
	// TODO: re-add an IPC identity guard once the connect IPC carries human_user_id.

	s, err := d.newAgentInstance(req)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}

	d.mu.Lock()
	d.instances[s.aiAgentInstanceID] = s
	d.mu.Unlock()

	// Send success response to client
	sendControl(conn, ipcResponse{
		Type:              "agent_instance_started",
		AIAgentInstanceID: s.aiAgentInstanceID,
	})

	// Enter binary I/O relay phase — blocks until instance ends or client disconnects
	s.AttachClient(conn)

	// Clean up instance after client disconnects
	d.mu.Lock()
	delete(d.instances, s.aiAgentInstanceID)
	d.mu.Unlock()

	s.Stop()
	log.Printf("daemon: agent instance %s ended", s.aiAgentInstanceID)
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
	// Check for active instances first
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is not running\n")
		return
	}

	// Query status to check for active instances
	statusMsg, _ := json.Marshal(ipcRequest{Type: "status"})
	statusMsg = append(statusMsg, '\n')
	conn.Write(statusMsg)

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	conn.Close()

	if err == nil {
		var status ipcResponse
		if json.Unmarshal(line, &status) == nil && len(status.Instances) > 0 {
			fmt.Fprintf(os.Stderr, "greenlight: %d active instance(s) will be terminated:\n", len(status.Instances))
			for _, s := range status.Instances {
				fmt.Fprintf(os.Stderr, "  %-10s %-8s %s\n",
					s.AIAgentInstanceID[:min(10, len(s.AIAgentInstanceID))], s.Agent, s.Project)
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

	if len(resp.Instances) == 0 {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is running (pid %s), no active instances\n", pid)
	} else {
		fmt.Fprintf(os.Stderr, "greenlight: daemon is running (pid %s), %d active instance(s):\n", pid, len(resp.Instances))
		for _, s := range resp.Instances {
			fmt.Fprintf(os.Stderr, "  %s  %s  %s  %s\n", s.AIAgentInstanceID[:8], s.Agent, s.Project, s.Cwd)
		}
	}
}
