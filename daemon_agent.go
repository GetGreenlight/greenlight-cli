//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// spawnedAgent tracks one detached agent process owned by the daemon.
type spawnedAgent struct {
	id      string // ai_agent_instances.id
	name    string
	pid     int
	cmd     *exec.Cmd
	logPath string
}

// daemonAgents is the daemon's in-memory registry of spawned agent processes.
// Keyed by ai_agent_instance_id. Populated when org agent create succeeds.
var (
	daemonAgents   = map[string]*spawnedAgent{}
	daemonAgentsMu sync.RWMutex
)

// killAllSpawnedAgents signals every tracked agent process so children don't
// outlive the daemon. Called from Daemon.Shutdown.
func killAllSpawnedAgents() {
	daemonAgentsMu.Lock()
	agents := make([]*spawnedAgent, 0, len(daemonAgents))
	for _, sa := range daemonAgents {
		agents = append(agents, sa)
	}
	daemonAgentsMu.Unlock()

	for _, sa := range agents {
		if sa.cmd == nil || sa.cmd.Process == nil {
			continue
		}
		log.Printf("daemon: killing agent %s (pid %d) on shutdown", sa.id, sa.pid)
		_ = sa.cmd.Process.Kill()
	}
}

// localAgentForHarness translates a server-side harness name (the value in
// harnesses.name) to the CLI's local agent identifier used by buildAgentCommand.
// Returns "" when no local binary maps to the harness.
func localAgentForHarness(harness string) string {
	switch harness {
	case "claude-code":
		return "claude"
	case "codex":
		return "codex"
	case "cursor":
		return "cursor"
	case "gemini":
		return "gemini"
	default:
		// windsurf, openai-assistants, etc. have no local CLI binary.
		return ""
	}
}

// handleCreateAgentInstance is the special-cased ws_request handler for
// create_ai_agent_instance. It pre-checks that the agent's working directory
// lives on this host (aborting before any row is created if not), forwards the
// create message to the server, then spawns the agent process detached.
func (d *Daemon) handleCreateAgentInstance(conn net.Conn, req ipcRequest) {
	// Parse the create payload to find the organization_position_id we need
	// to pre-check. The server validates everything else.
	var payload struct {
		OrganizationPositionID string `json:"organization_position_id"`
	}
	if err := json.Unmarshal(req.WSData, &payload); err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "invalid create payload: " + err.Error()})
		return
	}
	if payload.OrganizationPositionID == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "organization_position_id required"})
		return
	}

	// Step 1: resolve position → working_directory_id
	posReq, _ := json.Marshal(map[string]string{"id": payload.OrganizationPositionID})
	posResp, err := d.daemonWS.SendWSRequest(generateUUID(), "get_organization_position", posReq)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "failed to fetch organization_position: " + err.Error()})
		return
	}
	var posWrap struct {
		OrganizationPosition struct {
			WorkingDirectoryID string `json:"working_directory_id"`
		} `json:"organization_position"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(posResp, &posWrap); err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "invalid organization_position response: " + err.Error()})
		return
	}
	if posWrap.Error != "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "organization_position lookup: " + posWrap.Error})
		return
	}
	if posWrap.OrganizationPosition.WorkingDirectoryID == "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "organization_position has no working_directory"})
		return
	}

	// Step 2: resolve working_directory → host_id (and the path while we're here)
	wdReq, _ := json.Marshal(map[string]string{"id": posWrap.OrganizationPosition.WorkingDirectoryID})
	wdResp, err := d.daemonWS.SendWSRequest(generateUUID(), "get_working_directory", wdReq)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "failed to fetch working_directory: " + err.Error()})
		return
	}
	var wdWrap struct {
		WorkingDirectory struct {
			HostID        string `json:"host_id"`
			DirectoryPath string `json:"directory_path"`
		} `json:"working_directory"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(wdResp, &wdWrap); err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "invalid working_directory response: " + err.Error()})
		return
	}
	if wdWrap.Error != "" {
		sendControl(conn, ipcResponse{Type: "error", Message: "working_directory lookup: " + wdWrap.Error})
		return
	}

	// Hard-fail before creating the row if the agent isn't local to this daemon.
	if wdWrap.WorkingDirectory.HostID != d.sessionID {
		sendControl(conn, ipcResponse{
			Type: "error",
			Message: fmt.Sprintf("agent's working_directory is on host %s but this daemon is %s — refusing to create",
				wdWrap.WorkingDirectory.HostID, d.sessionID),
		})
		return
	}

	// Step 3: forward the create to the server (now safe).
	createResp, err := d.daemonWS.SendWSRequest(generateUUID(), "create_ai_agent_instance", req.WSData)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: err.Error()})
		return
	}
	var createWrap struct {
		AIAgentInstance struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"ai_agent_instance"`
		SpawnContext struct {
			HarnessName   string `json:"harness_name"`
			HostID        string `json:"host_id"`
			DirectoryPath string `json:"directory_path"`
		} `json:"spawn_context"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(createResp, &createWrap); err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "invalid create response: " + err.Error()})
		return
	}
	if createWrap.Error != "" {
		// Pass the server error through unchanged.
		sendControl(conn, ipcResponse{Type: "ws_response", Data: createResp})
		return
	}

	// Step 4: spawn detached.
	pid, logPath, spawnErr := d.spawnDetachedAgent(
		createWrap.AIAgentInstance.ID,
		createWrap.AIAgentInstance.Name,
		createWrap.SpawnContext.HarnessName,
		createWrap.SpawnContext.DirectoryPath,
	)

	// Build the augmented response: server payload + spawn outcome.
	out := map[string]interface{}{
		"ai_agent_instance": createWrap.AIAgentInstance,
		"spawn_context":     createWrap.SpawnContext,
	}
	if spawnErr != nil {
		log.Printf("daemon: agent %s row created but spawn failed: %v", createWrap.AIAgentInstance.ID, spawnErr)
		out["spawn_error"] = spawnErr.Error()
	} else {
		out["spawn"] = map[string]interface{}{
			"pid":      pid,
			"log_path": logPath,
		}
	}
	merged, err := json.Marshal(out)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "failed to marshal response: " + err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "ws_response", Data: merged})
}

// spawnDetachedAgent forks the harness binary into a detached child process,
// piping stdout/stderr into ~/.greenlight/agents/<id>.log. Returns the new pid
// and the log path. The child runs in its own session (Setsid) so it isn't
// attached to the calling terminal.
func (d *Daemon) spawnDetachedAgent(agentID, agentName, harnessName, cwd string) (int, string, error) {
	localAgent := localAgentForHarness(harnessName)
	if localAgent == "" {
		return 0, "", fmt.Errorf("no local CLI binary maps to harness %q", harnessName)
	}

	setup, err := buildAgentCommand(localAgent, "")
	if err != nil {
		return 0, "", fmt.Errorf("buildAgentCommand: %w", err)
	}

	// Open per-agent log file.
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, "", fmt.Errorf("UserHomeDir: %w", err)
	}
	logDir := filepath.Join(home, ".greenlight", "agents")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return 0, "", fmt.Errorf("mkdir %s: %w", logDir, err)
	}
	logPath := filepath.Join(logDir, agentID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", logPath, err)
	}

	cmd := exec.Command(setup.Command, setup.Args...)
	cmd.Dir = cwd
	cmd.Stdin = nil // effectively /dev/null
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Deliberately *not* using Setsid: we want the child to share the daemon's
	// process group so it dies when the daemon dies. The daemon also explicitly
	// kills tracked agents in Shutdown() as a safety net for clean exits.
	cmd.Env = append(os.Environ(),
		"GREENLIGHT_AGENT_INSTANCE_ID="+agentID,
		"GREENLIGHT_AGENT_NAME="+agentName,
	)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, "", fmt.Errorf("start: %w", err)
	}

	pid := cmd.Process.Pid
	sa := &spawnedAgent{
		id:      agentID,
		name:    agentName,
		pid:     pid,
		cmd:     cmd,
		logPath: logPath,
	}

	daemonAgentsMu.Lock()
	daemonAgents[agentID] = sa
	daemonAgentsMu.Unlock()

	// Reap the child in the background so the OS doesn't hold a zombie and
	// the in-memory registry stays consistent.
	go func() {
		err := cmd.Wait()
		logFile.Close()
		daemonAgentsMu.Lock()
		delete(daemonAgents, agentID)
		daemonAgentsMu.Unlock()
		if err != nil {
			log.Printf("daemon: agent %s (pid %d) exited: %v", agentID, pid, err)
		} else {
			log.Printf("daemon: agent %s (pid %d) exited cleanly", agentID, pid)
		}
	}()

	log.Printf("daemon: spawned agent %s (pid %d, log %s)", agentID, pid, logPath)
	return pid, logPath, nil
}
