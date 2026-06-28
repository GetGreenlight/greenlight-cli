//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Session represents a single agent session owned by the daemon.
type Session struct {
	relayID   string
	agent     string
	project   string
	cwd       string
	deviceID  string
	startedAt time.Time
	daemon    *Daemon

	relay *Relay

	interposeSock  string
	interposeClean func()
	interposeRelay *interposeRelay

	bridgePath     string
	bridgeDone     chan struct{}
	bridgeFinished chan struct{}

	transcriptCancel context.CancelFunc
	convID           string // set by startTranscriptStreamer, read at exit

	// Optional ticket reference stamped at launch when the session was spawned
	// from a phone Tickets-tab `+`. Echoed in session_start and persisted in
	// the session record so the UI can group sessions by ticket.
	ticket *TicketRef

	connectPidFile string
	libPath        string
	libExtracted   bool

	// Per-session bin dir containing a `greenlight` symlink pointing at the
	// running binary; prepended to the child agent's PATH.
	cliShimDir   string
	cliShimClean func()

	// Client connection (nil if detached)
	client   net.Conn
	clientMu sync.Mutex

	// Prompt proxying: when interpose needs a prompt, the daemon sends it
	// to the client and waits for a response.
	promptCh chan byte

	// Autopilot (#142): the stage prompt to inject as the session's first user
	// message once the agent is up. Empty for ordinary sessions.
	initialPrompt string
}

// newSession creates and starts a new agent session. This is the daemon-side
// equivalent of runConnect — it sets up the PTY, WebSocket, interpose, bridge,
// and transcript streamer, but does NOT touch the terminal (that's the client's job).
func (d *Daemon) newSession(req ipcRequest) (*Session, error) {
	if wsURL == "" {
		return nil, fmt.Errorf("no relay server URL configured")
	}

	cwd := req.Cwd
	if cwd == "" {
		return nil, fmt.Errorf("working directory is required")
	}

	// Resolve device ID and project first, so a project-scoped `agent` config
	// override can shadow the host default.
	devID, proj, err := resolveDeviceAndProject(req.DeviceID, req.Project, cwd)
	if err != nil {
		return nil, err
	}

	agent := resolveAgentForProject(req.Agent, proj)
	if !knownAgents[agent] {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}

	// On resume, carry the session's name and ticket forward from its saved
	// record. For fresh sessions, the ticket arrives via the
	// GREENLIGHT_TICKET_JSON env var set by the daemon when spawning the
	// terminal in response to a phone-initiated new_session.
	var sessionName string
	var ticket *TicketRef
	if req.Resume != "" {
		if rec, err := loadSessionRecord(req.Resume); err == nil {
			sessionName = rec.Name
			ticket = rec.Ticket
		}
	} else if blob := req.Env["GREENLIGHT_TICKET_JSON"]; blob != "" {
		var t TicketRef
		if err := json.Unmarshal([]byte(blob), &t); err == nil && t.Provider != "" {
			ticket = &t
		}
	}
	// Autopilot (#142): the orchestrator spawns with a role-derived name and a
	// stage prompt. The name overrides the auto-derived one; the prompt is
	// injected as the first user message once the agent is up (see runRelay).
	if req.Resume == "" {
		if n := req.Env["GREENLIGHT_SESSION_NAME"]; n != "" {
			sessionName = n
		}
	}
	// Autopilot stage prompt. Prefer the file handoff (#4) — the daemon writes
	// the prompt to a temp file and exports its path, so prompt prose never
	// traverses the shellQuote + AppleScript-typing layers that spawn the
	// terminal. Read it, then unlink so it doesn't linger in $TMPDIR. Fall back
	// to the inline GREENLIGHT_INITIAL_PROMPT var for compatibility.
	initialPrompt := req.Env["GREENLIGHT_INITIAL_PROMPT"]
	if pf := req.Env["GREENLIGHT_INITIAL_PROMPT_FILE"]; pf != "" {
		if data, err := os.ReadFile(pf); err == nil {
			initialPrompt = string(data)
		} else {
			log.Printf("daemon: failed to read initial prompt file %s: %v", pf, err)
		}
		os.Remove(pf)
	}

	// Resolve the active command shims (gh, glab, …) before the system prompt
	// is built, so the prompt can name the pre-authenticated commands (#198).
	// The actual PATH-shim install + display registration happen later (they
	// need shimDir from setupCLIShim), but both read this same slice, so the
	// prompt and the installed shim can never diverge. presentSecretNames
	// degrades to nil if the daemon WS is unavailable, so this stays empty
	// rather than blocking the session.
	var activeShim []resolvedShim
	if shimsEnabled() {
		present := d.presentSecretNames()
		activeShim = activeShims(present, configuredShimOverrides(proj, present))
	}

	// Build agent command with session IDs and flags
	setup, err := buildAgentCommand(agent, req.Resume, ticket, activeShim)
	if err != nil {
		return nil, err
	}
	command := setup.Command
	cmdArgs := setup.Args
	relayID := setup.RelayID

	// Register session with the server via daemon WS (no phone approval needed)
	if d.daemonWS == nil {
		return nil, fmt.Errorf("daemon WebSocket not connected")
	}
	idleNotifyAfterSecs := resolveIdleNotifyAfterSecs(proj)
	// Trusted-root set (#119): git worktrees (scoped) + OS scratch dirs
	// (ephemeral, gated by scratch_auto). Sent on session_start and re-sent on
	// mid-session worktree changes.
	rootsMgr := newSessionRootsManager(d.daemonWS, relayID, cwd, scratchAutoEnabled(proj))
	// Capture the start time before registering so it rides session_start and is
	// reused for the Session record below. The server emits it for live sessions
	// so the apps can order concurrent sessions chronologically (#154).
	startedAt := time.Now()
	skills, err := d.daemonWS.StartSession(relayID, proj, agentServerName(agent), cwd, version, sessionName, ticket, idleNotifyAfterSecs, startedAt, rootsMgr.snapshot())
	if err != nil {
		return nil, fmt.Errorf("session start failed: %w", err)
	}

	installAgentFiles(agent, relayID, cwd, ticket, activeShim)
	installSkills(agent, cwd, skills)

	s := &Session{
		relayID:       relayID,
		agent:         agent,
		project:       proj,
		cwd:           cwd,
		deviceID:      devID,
		startedAt:     startedAt,
		promptCh:      make(chan byte, 32),
		daemon:        d,
		ticket:        ticket,
		initialPrompt: initialPrompt,
	}

	// Write connect PID file
	s.connectPidFile = writeConnectPid(relayID, agent, cwd)

	// Create bridge file
	s.bridgePath = filepath.Join(os.TempDir(), "greenlight-bridge-"+relayID)
	if f, err := os.Create(s.bridgePath); err == nil {
		f.Close()
	}

	// Re-marshal the in-scope ticket so the agent's child env carries it
	// uniformly for both fresh (env-supplied) and resumed (record-supplied)
	// sessions, letting `greenlight ticket start|submit` default to it.
	var ticketJSON string
	if ticket != nil {
		if blob, err := json.Marshal(ticket); err == nil {
			ticketJSON = string(blob)
		}
	}
	exportEnvs := buildExportEnvs(devID, relayID, proj, s.bridgePath, agent, ticketJSON)
	if shimDir, cleanup := setupCLIShim(relayID); shimDir != "" {
		s.cliShimDir = shimDir
		s.cliShimClean = cleanup
		// Transparently wrap known token-authenticated commands (gh, glab, …)
		// whose secret is stored, so the agent runs them bare while greenlight
		// injects the token via `greenlight run`. Resolved above (activeShim) so
		// the PATH shim and the system-prompt "pre-authenticated" line stay in
		// lockstep (#198).
		if len(activeShim) > 0 {
			installCommandShims(shimDir, activeShim)
			setActiveShims(activeShim)
		}
	}
	command, cmdArgs, interpose, err := setupInterpose(agent, command, cmdArgs, relayID, cwd, exportEnvs)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("interpose setup failed: %w", err)
	}
	s.libPath = interpose.LibPath
	s.libExtracted = interpose.LibExtracted
	s.interposeSock = interpose.SockPath
	s.interposeClean = interpose.SockCleanup
	s.interposeRelay = interpose.Relay

	// Create the relay (PTY only, no per-session WebSocket) in daemon mode
	r, err := NewDaemon(command, cmdArgs, exportEnvs, cwd, req.Winsize, req.Env, s.cliShimDir, agent)
	if err != nil {
		s.cleanup()
		return nil, fmt.Errorf("failed to create relay: %w", err)
	}
	s.relay = r

	// Register this session with the daemon's shared WebSocket
	killFunc := func() {
		r.killed = true
		s.Stop()
	}
	if d.daemonWS != nil {
		sw := d.daemonWS.RegisterSession(relayID, r.Inject, killFunc)
		sw.project = proj
		sw.agent = agentServerName(agent)
		sw.localAgent = agent
		sw.cwd = cwd
		sw.version = version
		// r.screen is non-nil only for suggestion-capable agents (see
		// NewDaemon), so its presence is the gate.
		if r.screen != nil {
			sw.suggestionFunc = r.screen.suggestion
		}
		if sessionName != "" {
			sw.name = sessionName
			sw.nameSet = true
		}
		r.wsConn = sw

		// Start bridge tailer using the session's WS handle
		s.bridgeDone = make(chan struct{})
		s.bridgeFinished = make(chan struct{})
		go func() {
			tailBridge(s.bridgePath, sw, s.bridgeDone, agentServerName(agent))
			close(s.bridgeFinished)
		}()
	}

	// Set up prompt relay so interpose can show prompts
	if s.interposeRelay != nil {
		s.interposeRelay.SetRelay(r)
		// Re-report trusted roots when the agent adds/removes/moves a worktree
		// (issue #119, trigger 2).
		s.interposeRelay.SetWorktreeHandler(rootsMgr.onWorktreeChange)
		// Lazily reconcile roots when a file op targets a path outside the known
		// set — the durable net for a mid-session worktree (#119 trigger 3 / #148).
		s.interposeRelay.SetReconcileHandler(rootsMgr.reconcileForPath)
	}

	// Start transcript streamer
	startTime := time.Now()
	var transcriptCtx context.Context
	transcriptCtx, s.transcriptCancel = context.WithCancel(context.Background())
	go startTranscriptStreamer(transcriptCtx, agent, relayID, setup.AgentSessionID, s.bridgePath, cwd, startTime, &s.convID, rootsMgr.addScratchpadRoot)

	// Note: don't start the relay yet — wait until a client attaches
	// so no PTY output is lost. runRelay() is called from AttachClient.

	return s, nil
}

// runRelay runs the PTY child process. When it exits, the session is done.
func (s *Session) runRelay() {
	// Autopilot (#142): inject the stage prompt as the first user message once
	// the agent's TUI has had a moment to come up. Best-effort: agents buffer
	// stdin, so a modest delay after the PTY starts reliably lands the prompt;
	// failure (agent not ready / no prompt) is harmless and just logged.
	if s.initialPrompt != "" {
		go s.injectInitialPrompt()
	}
	err := s.relay.RunDaemon()

	s.transcriptCancel()
	killStreamer(s.relayID)
	s.finishRelay(err)
}

// injectInitialPrompt delivers the autopilot stage prompt into the agent's PTY
// as the first user message. Typing into the composer before the agent's TUI is
// interactive is the cause of the "prompt typed but never submitted" failure
// (#145): the first PTY bytes are terminal-setup escapes, not a ready input box,
// so a fixed settle after them is unreliable. Instead we wait for the agent to
// start producing output (relay.readyCh) and then for that output to go quiet —
// the TUI has finished its initial paint and is waiting for input. This adapts
// to fast and slow starts alike. Both waits are bounded so a pathological start
// still delivers rather than hanging. Delivery uses the shared submitInput path,
// but with a wider Enter gap than steady-state phone input because a
// just-rendered composer needs a longer beat to register the submit and not
// fold the trailing Enter into the pasted text.
func (s *Session) injectInitialPrompt() {
	// Wait for the agent to begin painting; safety net for a no-output start.
	const firstOutputTimeout = 10 * time.Second
	// How long PTY output must stay quiet to consider the TUI settled.
	const quietPeriod = 750 * time.Millisecond
	// Overall cap on the quiet-wait so a perpetually chatty start still delivers.
	const settleDeadline = 20 * time.Second
	// Enter gap for a freshly rendered composer — wider than the phone path's.
	const initialSubmitGap = 300 * time.Millisecond

	select {
	case <-s.relay.readyCh: // agent has produced output — TUI is coming up.
	case <-time.After(firstOutputTimeout):
		log.Printf("daemon: autopilot prompt readiness timed out (relay %s); delivering anyway", s.relayID)
	case <-s.relay.shutdownCh: // session ended before the agent rendered.
		return
	}

	// Wait for output to fall quiet for quietPeriod — i.e. the TUI has stopped
	// repainting and is idle at the input prompt — bounded by settleDeadline.
	deadline := time.Now().Add(settleDeadline)
	for {
		quietFor := time.Since(time.Unix(0, s.relay.lastOutputAt.Load()))
		if quietFor >= quietPeriod {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("daemon: autopilot prompt settle deadline reached (relay %s); delivering anyway", s.relayID)
			break
		}
		time.Sleep(quietPeriod - quietFor)
	}

	if err := submitInputGap(s.relay.Inject, []byte(s.initialPrompt), initialSubmitGap); err != nil {
		log.Printf("daemon: autopilot prompt delivery failed (relay %s): %v", s.relayID, err)
		return
	}
	log.Printf("daemon: autopilot injected initial prompt into relay %s (%d bytes)", s.relayID, len(s.initialPrompt))
}

// finishRelay carries out the post-exit bookkeeping for a relay that has stopped.
func (s *Session) finishRelay(err error) {
	if s.bridgeDone != nil {
		close(s.bridgeDone)
		<-s.bridgeFinished
	}

	// Persist session state for potential future resume/wake
	saveSessionRecord(s)

	// Send exit frame to attached client
	s.clientMu.Lock()
	client := s.client
	s.clientMu.Unlock()

	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	if client != nil {
		convID := s.convID
		exitMsg := struct {
			Code           int    `json:"code"`
			ConversationID string `json:"conversation_id,omitempty"`
			Agent          string `json:"agent,omitempty"`
		}{
			Code:           exitCode,
			ConversationID: convID,
			Agent:          s.agent,
		}
		exitData, _ := json.Marshal(exitMsg)
		writeFrame(client, frameExit, exitData)
	}

	log.Printf("daemon: session %s relay exited (err=%v)", s.relayID, err)
}

// AttachClient enters the binary I/O relay phase with the given client connection.
// Blocks until the client disconnects or the session ends.
func (s *Session) AttachClient(conn net.Conn) {
	s.clientMu.Lock()
	s.client = conn
	s.clientMu.Unlock()

	defer func() {
		s.clientMu.Lock()
		s.client = nil
		s.clientMu.Unlock()
	}()

	// Set up the relay's daemon writer to send PTY output to this client,
	// then start the child process. Order matters: the writer must be set
	// before RunDaemon starts so no PTY output is lost.
	s.relay.setDaemonWriter(conn)
	defer s.relay.setDaemonWriter(nil)

	// Start the child process now that we have a client to receive output
	go s.runRelay()

	// Read frames from client until disconnect or exit
	for {
		frameType, payload, err := readFrame(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("daemon: client read error: %v", err)
			}
			return
		}

		switch frameType {
		case frameStdin:
			s.relay.Inject(payload)

		case frameResize:
			var ws ipcWinsize
			if json.Unmarshal(payload, &ws) == nil {
				rows := ws.Rows
				if rows > promptHeight {
					rows -= promptHeight
				}
				winsize := &Winsize{Row: rows, Col: ws.Cols}
				setWinsize(s.relay.master.Fd(), winsize)
				s.relay.screen.resize(int(ws.Cols), int(rows))
			}

		case frameSignal:
			var sig struct {
				Signal string `json:"signal"`
			}
			if json.Unmarshal(payload, &sig) == nil && s.relay.cmd.Process != nil {
				switch sig.Signal {
				case "INT":
					s.relay.cmd.Process.Signal(syscall.SIGINT)
				case "TERM":
					s.relay.cmd.Process.Signal(syscall.SIGTERM)
				}
			}

		case framePromptResp:
			// Forward prompt keystrokes to the relay's prompt channel
			for _, b := range payload {
				select {
				case s.relay.promptCh <- b:
				default:
				}
			}

		case frameExit:
			return
		}
	}
}

// Stop terminates the session, killing the child process and cleaning up.
func (s *Session) Stop() {
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

// cleanup removes session-specific files and resources.
func (s *Session) cleanup() {
	if s.interposeClean != nil {
		s.interposeClean()
	}
	if s.libPath != "" && s.libExtracted {
		os.Remove(s.libPath)
	}
	if s.cliShimClean != nil {
		s.cliShimClean()
	}
	if s.connectPidFile != "" {
		os.Remove(s.connectPidFile)
		cleanupAgentFiles(s.agent, s.cwd)
	}
	if s.bridgePath != "" {
		os.Remove(s.bridgePath)
	}

	if s.interposeRelay != nil {
		s.interposeRelay.ClearRelay(s.relay)
	}

	// Notify server and unregister from daemon's shared WebSocket
	if s.daemon != nil && s.daemon.daemonWS != nil {
		s.daemon.daemonWS.EndSession(s.relayID)
		s.daemon.daemonWS.UnregisterSession(s.relayID)
	}
}

// NewDaemon creates a Relay configured for daemon mode. The PTY is created
// but terminal raw mode is NOT set (the client handles that). The child
// process's working directory is set to cwd.
func NewDaemon(command string, args []string, exportEnvs map[string]string, cwd string, winsize *ipcWinsize, clientEnv map[string]string, cliShimDir string, agent string) (*Relay, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("openPTY: %w", err)
	}

	// Set initial window size if provided.
	// Reserve promptHeight rows at the bottom of the client terminal for
	// permission prompts so the agent never renders into that area.
	if winsize != nil {
		rows := winsize.Rows
		if rows > promptHeight {
			rows -= promptHeight
		}
		ws := &Winsize{Row: rows, Col: winsize.Cols}
		setWinsize(master.Fd(), ws)
	}

	// Use client's env as base if available, otherwise daemon's env.
	// Build the full env first so we can resolve the command using the client's PATH.
	var childEnv []string
	if clientEnv != nil {
		for k, v := range clientEnv {
			childEnv = append(childEnv, k+"="+v)
		}
	} else {
		childEnv = os.Environ()
	}
	// Prepend the per-session CLI shim dir to PATH so that any `greenlight`
	// invocation by the agent resolves to *this* binary, regardless of what
	// else is on the user's PATH. Done before exportEnvs so the rest of the
	// env-building stays unchanged.
	if cliShimDir != "" {
		pathFound := false
		for i, e := range childEnv {
			if k, v, ok := strings.Cut(e, "="); ok && k == "PATH" {
				childEnv[i] = "PATH=" + prependPATH(cliShimDir, v)
				pathFound = true
				break
			}
		}
		if !pathFound {
			childEnv = append(childEnv, "PATH="+cliShimDir)
		}
	}
	// Override with greenlight-specific vars
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
		cmd:        cmd,
		master:     master,
		slave:      slave,
		promptCh:   make(chan byte, 32),
		readyCh:    make(chan struct{}),
		daemonMode: true,
	}

	// PTY screen tap (issue #38): a virtual terminal fed every PTY byte so the
	// composer ghost-suggestion can be read on demand. Created only for agents
	// that support extraction (claude today) — the model can't be reconstructed
	// retroactively so it must observe from session start, but feeding vt10x +
	// the per-Write faint rewrite for an agent that can never produce a
	// suggestion is pure overhead on the hot output path.
	if agentSupportsSuggestions(agent) {
		cols, rows := 80, 24
		if ws, err := getWinsize(master.Fd()); err == nil {
			cols, rows = int(ws.Col), int(ws.Row)
		}
		r.screen = newScreenTap(cols, rows)
	}

	return r, nil
}

// RunDaemon starts the child process and relays PTY output.
// Unlike Run(), it does NOT set terminal raw mode or read from os.Stdin.
// PTY output is sent to the daemonWriter (client connection) and WebSocket.
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

	// Mark ready for prompts
	r.promptReady.Store(true)

	// PTY output relay: master → daemonWriter + WebSocket
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.master.Read(buf)
			if n > 0 {
				data := buf[:n]

				// First output from the agent means its TUI is coming up — the
				// CLI-side analog of the phone's transcript view appearing once
				// the session starts. Signal readiness so a pending autopilot
				// initial-prompt injection can proceed (see injectInitialPrompt).
				r.readyOnce.Do(func() { close(r.readyCh) })

				// Track the latest output time so injectInitialPrompt can wait
				// for the TUI to stop painting before it types and submits.
				r.lastOutputAt.Store(time.Now().UnixNano())

				// Feed the experimental screen model (observe-only, #38).
				if r.screen != nil {
					r.screen.Write(data)
				}

				// Send to attached client via daemon writer
				r.daemonMu.RLock()
				w := r.daemonWriter
				r.daemonMu.RUnlock()
				if w != nil {
					writeFrame(w, frameStdout, data)
				}
			}
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

// setDaemonWriter sets or clears the writer for PTY output in daemon mode.
func (r *Relay) setDaemonWriter(w io.Writer) {
	r.daemonMu.Lock()
	r.daemonWriter = w
	r.daemonMu.Unlock()
}

// newAgentCmd wraps exec.Command — exists to allow Cursor workarounds etc.
func newAgentCmd(command string, args []string) *exec.Cmd {
	return exec.Command(command, args...)
}

// ShowPromptDaemon sends a prompt to the attached client and waits for response.
// This is the daemon-mode equivalent of ShowPrompt.
func (r *Relay) ShowPromptDaemon(ctx context.Context, toolName, detail string) (int, error) {
	r.daemonMu.RLock()
	w := r.daemonWriter
	r.daemonMu.RUnlock()

	if w == nil {
		return -1, fmt.Errorf("no client attached")
	}

	r.promptMu.Lock()
	defer r.promptMu.Unlock()

	// Send prompt frame to client
	promptData, _ := json.Marshal(map[string]string{
		"tool_name": toolName,
		"detail":    detail,
	})
	if err := writeFrame(w, framePrompt, promptData); err != nil {
		return -1, err
	}

	// Drain stale keystrokes
	for {
		select {
		case <-r.promptCh:
		default:
			goto drained
		}
	}
drained:

	// Wait for response from client
	for {
		select {
		case <-ctx.Done():
			// Server won the race — tell client to clear the prompt
			r.daemonMu.RLock()
			cw := r.daemonWriter
			r.daemonMu.RUnlock()
			if cw != nil {
				writeFrame(cw, framePromptCancel, nil)
			}
			return -1, ctx.Err()
		case b := <-r.promptCh:
			switch b {
			case '1':
				return 0, nil
			case '2':
				return 1, nil
			case '3':
				return 2, nil
			case '4':
				return 3, nil
			}
		}
	}
}

// resolveIdleNotifyAfterSecs returns the idle_notify_after config value for the
// given project, converted to seconds. Returns 0 if not set, invalid, or < 1m.
func resolveIdleNotifyAfterSecs(project string) int {
	v := resolveConfig(project, configKeyIdleNotifyAfter)
	if v == "" || v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		return 0
	}
	return int(d.Seconds())
}
