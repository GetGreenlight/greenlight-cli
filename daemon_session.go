//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AgentInstance represents a single running agent owned by the daemon.
// Each AgentInstance corresponds exactly to one ai_agent_instance_id (one
// conversation) on the server. All instances are detached: the PTY runs
// in the background and the user interacts via the iOS app or
// `greenlight talk`.
type AgentInstance struct {
	aiAgentInstanceID string
	agent             string
	project           string
	cwd               string
	deviceID          string
	startedAt         time.Time
	daemon            *Daemon

	relay *Relay

	interposeSock  string
	interposeClean func()
	interposeRelay *interposeRelay

	bridgePath     string
	bridgeDone     chan struct{}
	bridgeFinished chan struct{}

	transcriptCancel context.CancelFunc

	libPath      string
	libExtracted bool
}

// newAgentInstance creates and starts a new agent instance. It sets up the
// PTY, WebSocket,
// interpose, bridge, and transcript streamer, but does NOT touch the terminal
// (that's the client's job).
func (d *Daemon) newAgentInstance(req ipcRequest) (*AgentInstance, error) {
	if wsURL == "" {
		return nil, fmt.Errorf("no relay server URL configured")
	}

	agent := resolveAgent(req.Agent)
	if !knownAgents[agent] {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}

	cwd := req.Cwd
	if cwd == "" {
		return nil, fmt.Errorf("working directory is required")
	}

	// The agent's "device_id" is the daemon's human_user_id. Fall back to
	// env/config only if the daemon hasn't resolved one yet (shouldn't happen
	// once the daemon is registered).
	devID := d.humanUserID
	if devID == "" {
		devID = os.Getenv("GREENLIGHT_DEVICE_ID")
	}
	if devID == "" {
		devID = readConfigValue("device_id")
	}
	if devID == "" {
		return nil, fmt.Errorf("not registered — run 'greenlight register <email>' first")
	}

	proj := req.Project
	if proj == "" {
		proj = os.Getenv("GREENLIGHT_PROJECT")
	}
	if proj == "" {
		proj = readConfigValue("project")
	}
	if proj == "" && cwd != "" {
		proj = filepath.Base(cwd)
	}

	// Build agent command with its agent-internal session ID and flags
	setup, err := buildAgentCommand(agent)
	if err != nil {
		return nil, err
	}
	command := setup.Command
	cmdArgs := setup.Args
	aiAgentInstanceID := setup.AIAgentInstanceID
	// Honor an explicit ai_agent_instance_id override (used by spawnAgentInstance
	// so the instance is keyed by the supplied ID rather than a fresh UUID).
	if req.AIAgentInstanceID != "" {
		aiAgentInstanceID = req.AIAgentInstanceID
		setup.AIAgentInstanceID = req.AIAgentInstanceID
		if agent == "codex" {
			// Codex reuses the instance ID as its agent-session sentinel.
			setup.AgentSessionID = req.AIAgentInstanceID
		}
	}

	// Register instance with the server via daemon WS (no phone approval needed)
	if d.daemonWS == nil {
		return nil, fmt.Errorf("daemon WebSocket not connected")
	}
	if err := d.daemonWS.ConnectAgentInstance(aiAgentInstanceID, proj, agentServerName(agent), cwd, version); err != nil {
		return nil, fmt.Errorf("agent_instance_connect failed: %w", err)
	}

	installAgentFiles(agent, aiAgentInstanceID)

	s := &AgentInstance{
		aiAgentInstanceID: aiAgentInstanceID,
		agent:             agent,
		project:           proj,
		cwd:               cwd,
		deviceID:          devID,
		startedAt:         time.Now(),
		daemon:            d,
	}

	// Create bridge file
	s.bridgePath = filepath.Join(os.TempDir(), "greenlight-bridge-"+aiAgentInstanceID)
	if f, err := os.Create(s.bridgePath); err == nil {
		f.Close()
	}

	exportEnvs := buildExportEnvs(devID, aiAgentInstanceID, proj, s.bridgePath, agent, req.ModelName)
	command, cmdArgs, interpose, err := setupInterpose(agent, command, cmdArgs, aiAgentInstanceID, cwd, exportEnvs)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("interpose setup failed: %w", err)
	}
	s.libPath = interpose.LibPath
	s.libExtracted = interpose.LibExtracted
	s.interposeSock = interpose.SockPath
	s.interposeClean = interpose.SockCleanup
	s.interposeRelay = interpose.Relay

	// Create the relay (PTY only, no per-instance WebSocket) in daemon mode
	r, err := NewDaemon(command, cmdArgs, exportEnvs, cwd, req.Winsize)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("failed to create relay: %w", err)
	}
	s.relay = r

	// Register this instance with the daemon's shared WebSocket
	killFunc := func() {
		if r.cmd.Process != nil {
			r.killed = true
			syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if d.daemonWS != nil {
		aw := d.daemonWS.RegisterAgentInstance(aiAgentInstanceID, r.Inject, killFunc)
		aw.project = proj
		aw.agent = agentServerName(agent)
		aw.cwd = cwd
		aw.version = version
		r.wsConn = aw

		// Start bridge tailer using the instance's WS handle
		s.bridgeDone = make(chan struct{})
		s.bridgeFinished = make(chan struct{})
		go func() {
			tailBridge(s.bridgePath, aw, s.bridgeDone, agentServerName(agent))
			close(s.bridgeFinished)
		}()
	}

	// Set up prompt relay so interpose can show prompts
	s.interposeRelay.SetRelay(r)

	// Start transcript streamer
	startTime := time.Now()
	var transcriptCtx context.Context
	transcriptCtx, s.transcriptCancel = context.WithCancel(context.Background())
	go startTranscriptStreamer(transcriptCtx, agent, aiAgentInstanceID, setup.AgentSessionID, s.bridgePath, cwd, startTime)

	// The caller is responsible for starting the PTY via s.runRelay() in a
	// goroutine so it can attach its own lifecycle cleanup (see daemon_agent.go's
	// spawnAgentInstance for the canonical pattern).

	return s, nil
}

// runRelay runs the PTY child process. When it exits, the instance is done.
func (s *AgentInstance) runRelay() {
	err := s.relay.RunDaemon()

	s.transcriptCancel()
	killStreamer(s.aiAgentInstanceID)

	if s.bridgeDone != nil {
		close(s.bridgeDone)
		<-s.bridgeFinished
	}

	log.Printf("daemon: agent instance %s relay exited (err=%v)", s.aiAgentInstanceID, err)
}

// Stop terminates the instance, killing the child process and cleaning up.
func (s *AgentInstance) Stop() {
	// The transcript streamer is a detached process (its own session) and
	// will outlive the daemon unless we reap it explicitly. Do this
	// synchronously — the relay-exit path also calls killStreamer, but that
	// goroutine isn't guaranteed to run before the daemon process exits.
	killStreamer(s.aiAgentInstanceID)

	if s.relay != nil && s.relay.cmd != nil && s.relay.cmd.Process != nil {
		s.relay.cmd.Process.Signal(syscall.SIGTERM)
		// Give it a moment, then force kill
		go func() {
			time.Sleep(3 * time.Second)
			if s.relay.cmd.Process != nil {
				s.relay.cmd.Process.Kill()
			}
		}()
	}
	s.cleanup()
}

// cleanup removes instance-specific files and resources.
func (s *AgentInstance) cleanup() {
	if s.interposeClean != nil {
		s.interposeClean()
	}
	if s.libPath != "" && s.libExtracted {
		os.Remove(s.libPath)
	}
	if s.bridgePath != "" {
		os.Remove(s.bridgePath)
	}

	if s.interposeRelay != nil {
		s.interposeRelay.ClearRelay(s.relay)
	}

	// Notify server and unregister from daemon's shared WebSocket
	if s.daemon != nil && s.daemon.daemonWS != nil {
		s.daemon.daemonWS.DisconnectAgentInstance(s.aiAgentInstanceID)
		s.daemon.daemonWS.UnregisterAgentInstance(s.aiAgentInstanceID)
	}
}

// NewDaemon creates a Relay configured for a detached agent instance.
// The PTY is created and its window size set from `winsize`; the child
// process's working directory is set to cwd.
func NewDaemon(command string, args []string, exportEnvs map[string]string, cwd string, winsize *ipcWinsize) (*Relay, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("openPTY: %w", err)
	}

	// Set initial window size if provided.
	if winsize != nil {
		ws := &Winsize{Row: winsize.Rows, Col: winsize.Cols}
		setWinsize(master.Fd(), ws)
	}

	// Build the child's environment: daemon's env + greenlight-specific vars.
	childEnv := os.Environ()
	for k, v := range exportEnvs {
		childEnv = append(childEnv, k+"="+v)
	}

	// Resolve the command binary using the child's PATH (not the daemon's).
	// exec.Command uses LookPath with the current process's PATH, which may
	// point to a different binary. Temporarily swap PATH for resolution.
	resolvedCmd := command
	for _, e := range childEnv {
		if k, v, ok := strings.Cut(e, "="); ok && k == "PATH" {
			origPath := os.Getenv("PATH")
			os.Setenv("PATH", v)
			if p, err := exec.LookPath(command); err == nil {
				resolvedCmd = p
			}
			os.Setenv("PATH", origPath)
			break
		}
	}

	cmd := newAgentCmd(resolvedCmd, args)
	cmd.Dir = cwd
	cmd.Env = childEnv

	r := &Relay{
		cmd:    cmd,
		master: master,
		slave:  slave,
	}

	return r, nil
}

// RunDaemon starts the child process, drains PTY output (no attached terminal),
// and returns when the child exits. Transcripts flow to the phone via the
// streamer + bridge file, not through the PTY.
func (r *Relay) RunDaemon() error {
	defer r.cleanupDaemon()

	// Start child process on the slave PTY
	r.cmd.Stdin = r.slave
	r.cmd.Stdout = r.slave
	r.cmd.Stderr = r.slave
	r.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    3,
	}
	r.cmd.ExtraFiles = []*os.File{r.slave}

	if err := r.cmd.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}
	log.Printf("daemon: child started (pid %d, cmd %s)", r.cmd.Process.Pid, r.cmd.Path)

	r.slave.Close()
	r.slave = nil

	// Drain PTY output into the void — nothing consumes it directly. The
	// agent's own transcript JSONL is the canonical source of user-visible
	// activity, read by the streamer and forwarded via WebSocket.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := r.master.Read(buf)
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// Wait for child to exit
	waitErr := r.cmd.Wait()

	r.master.Close()
	r.master = nil

	<-done

	return waitErr
}

func (r *Relay) cleanupDaemon() {
	if r.master != nil {
		r.master.Close()
	}
	if r.slave != nil {
		r.slave.Close()
	}
}

// newAgentCmd wraps exec.Command — exists to allow Cursor workarounds etc.
func newAgentCmd(command string, args []string) *exec.Cmd {
	return exec.Command(command, args...)
}
