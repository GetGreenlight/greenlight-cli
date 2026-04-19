//go:build darwin || linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"syscall"
)

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
// create message to the server, then spawns a full instance reachable from the
// phone and the `greenlight talk` TUI.
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
	if wdWrap.WorkingDirectory.HostID != d.hostID {
		sendControl(conn, ipcResponse{
			Type: "error",
			Message: fmt.Sprintf("agent's working_directory is on host %s but this daemon is %s — refusing to create",
				wdWrap.WorkingDirectory.HostID, d.hostID),
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
			ModelProvider string `json:"model_provider"`
			ModelName     string `json:"model_name"`
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

	// Step 4: spawn a full instance (relay-registered, transcript-streamed).
	pid, spawnErr := d.spawnAgentInstance(
		createWrap.AIAgentInstance.ID,
		createWrap.AIAgentInstance.Name,
		createWrap.SpawnContext.HarnessName,
		createWrap.SpawnContext.DirectoryPath,
		createWrap.SpawnContext.ModelProvider,
		createWrap.SpawnContext.ModelName,
	)

	out := map[string]interface{}{
		"ai_agent_instance": createWrap.AIAgentInstance,
		"spawn_context":     createWrap.SpawnContext,
	}
	if spawnErr != nil {
		log.Printf("daemon: agent %s row created but spawn failed: %v", createWrap.AIAgentInstance.ID, spawnErr)
		out["spawn_error"] = spawnErr.Error()
	} else {
		out["spawn"] = map[string]interface{}{
			"pid":                  pid,
			"ai_agent_instance_id": createWrap.AIAgentInstance.ID,
		}
		// Mark agent as active in the DB
		d.updateAgentStatus(createWrap.AIAgentInstance.ID, "active")
	}
	merged, err := json.Marshal(out)
	if err != nil {
		sendControl(conn, ipcResponse{Type: "error", Message: "failed to marshal response: " + err.Error()})
		return
	}
	sendControl(conn, ipcResponse{Type: "ws_response", Data: merged})
}

// spawnAgentInstance creates a fully-functioning agent instance with the full
// daemon machinery (interpose hook, transcript bridge, WS registration with
// the server). The PTY runs detached in the background; the phone and
// `greenlight talk` drive it.
//
// The instance's id is the ai_agent_instance_id, so 'org agent stop' can find
// and terminate it without any extra bookkeeping.
func (d *Daemon) spawnAgentInstance(agentInstanceID, agentName, harnessName, cwd, modelProvider, modelName string) (int, error) {
	localAgent := localAgentForHarness(harnessName)
	if localAgent == "" {
		return 0, fmt.Errorf("no local CLI binary maps to harness %q", harnessName)
	}

	// Ensure the agent's working directory exists. The default template is
	// $HOME/greenlight_agents which most users won't have yet; without this
	// the exec.Cmd.Dir = cwd below would fail with ENOENT.
	if cwd != "" {
		if err := os.MkdirAll(cwd, 0755); err != nil {
			return 0, fmt.Errorf("create working directory %s: %w", cwd, err)
		}
	}

	req := ipcRequest{
		Agent:             localAgent,
		Project:           agentName, // surface the agent name as the "project" pill
		Cwd:               cwd,
		AIAgentInstanceID: agentInstanceID,
		Winsize:           &ipcWinsize{Rows: 40, Cols: 120},
		ModelProvider:     modelProvider,
		ModelName:         modelName,
	}
	s, err := d.newAgentInstance(req)
	if err != nil {
		return 0, err
	}

	d.mu.Lock()
	d.instances[s.aiAgentInstanceID] = s
	d.mu.Unlock()

	// Run the relay PTY in the background. When the child exits, drop the
	// instance from the registry and report actual liveness via pid_status.
	// We leave status (intent) alone — that's set server-side on explicit
	// sleep/wake/retire commands.
	go func() {
		waitErr := s.runRelay()
		d.mu.Lock()
		delete(d.instances, s.aiAgentInstanceID)
		d.mu.Unlock()
		s.Stop()
		d.reportPIDStatus(s.aiAgentInstanceID, classifyExit(waitErr))
		log.Printf("daemon: spawned agent instance %s ended", s.aiAgentInstanceID)
	}()

	pid := 0
	if s.relay != nil && s.relay.cmd != nil && s.relay.cmd.Process != nil {
		pid = s.relay.cmd.Process.Pid
	}
	log.Printf("daemon: spawned agent instance %s (pid %d, cwd %s)", s.aiAgentInstanceID, pid, cwd)
	return pid, nil
}

// updateAgentStatus sends an update_ai_agent_instance message to the server
// to set the agent's status and host_id in the DB.
func (d *Daemon) updateAgentStatus(agentInstanceID, status string) {
	data, _ := json.Marshal(map[string]string{
		"id":      agentInstanceID,
		"status":  status,
		"host_id": d.hostID,
	})
	_, err := d.daemonWS.SendWSRequest(generateUUID(), "update_ai_agent_instance", data)
	if err != nil {
		log.Printf("daemon: failed to update agent %s status to %s: %v", agentInstanceID, status, err)
	} else {
		log.Printf("daemon: agent %s status updated to %s", agentInstanceID, status)
	}
}

// classifyExit maps a cmd.Wait() error to a pid_status value. A signal
// exit is 'killed' regardless of who sent the signal — the SoT for
// "was this user intent?" is the status (intent) column, not pid_status.
func classifyExit(err error) string {
	if err == nil {
		return "exited"
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "exited"
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return "killed"
		}
	}
	return "exited"
}

// reportPIDStatus sends the daemon's actual-liveness update to the
// server. Best-effort: if the WS isn't connected, we skip — the server
// will eventually flip pid_status='host_disconnected' when it notices
// the daemon drop.
func (d *Daemon) reportPIDStatus(agentInstanceID, pidStatus string) {
	if d.daemonWS == nil || !d.daemonWS.IsConnected() {
		return
	}
	data, _ := json.Marshal(map[string]string{
		"id":         agentInstanceID,
		"pid_status": pidStatus,
	})
	if _, err := d.daemonWS.SendWSRequest(generateUUID(), "set_ai_agent_instance_pid_status", data); err != nil {
		log.Printf("daemon: failed to report pid_status=%s for %s: %v", pidStatus, agentInstanceID, err)
	}
}

// handleSleepAgentInstance is called by the daemon WS when the server
// forwards a sleep command. The status flip is authoritative on the
// server; our job is just to tear down the local child process. No-op
// if the instance isn't currently running on this daemon.
func (d *Daemon) handleSleepAgentInstance(agentInstanceID string) {
	d.mu.Lock()
	s := d.instances[agentInstanceID]
	delete(d.instances, agentInstanceID)
	d.mu.Unlock()
	if s == nil {
		return
	}
	s.Stop()
}

// handleWakeAgentInstance is called by the daemon WS when the server
// forwards a wake command with spawn context. Spawns a fresh child
// process for the instance. If one is already running locally we
// short-circuit — wake is idempotent.
func (d *Daemon) handleWakeAgentInstance(agentInstanceID string, spawnCtx json.RawMessage) {
	d.mu.RLock()
	_, exists := d.instances[agentInstanceID]
	d.mu.RUnlock()
	if exists {
		log.Printf("daemon: wake %s: already running locally, ignoring", agentInstanceID)
		return
	}

	var ctx struct {
		HarnessName   string `json:"harness_name"`
		HostID        string `json:"host_id"`
		DirectoryPath string `json:"directory_path"`
		ModelProvider string `json:"model_provider"`
		ModelName     string `json:"model_name"`
		AgentName     string `json:"agent_name"`
	}
	if err := json.Unmarshal(spawnCtx, &ctx); err != nil {
		log.Printf("daemon: wake %s: invalid spawn_context: %v", agentInstanceID, err)
		return
	}
	if ctx.HostID != "" && d.hostID != "" && ctx.HostID != d.hostID {
		// Shouldn't happen — the server looked up our daemon via hostID
		// before forwarding — but guard anyway.
		log.Printf("daemon: wake %s: spawn_context host %s doesn't match this daemon %s", agentInstanceID, ctx.HostID, d.hostID)
		return
	}
	if _, err := d.spawnAgentInstance(agentInstanceID, ctx.AgentName, ctx.HarnessName, ctx.DirectoryPath, ctx.ModelProvider, ctx.ModelName); err != nil {
		log.Printf("daemon: wake %s: spawn failed: %v", agentInstanceID, err)
	}
}
