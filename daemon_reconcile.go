//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// agentWakeStagger spaces out concurrent spawns at daemon-start so that
// claude's slow warmup (plugin loading, auth refresh) doesn't thrash the
// machine. See docs/daemon-agent-lifecycle.md.
const agentWakeStagger = 250 * time.Millisecond

// wakeTargetPayload mirrors the ai_agent_instance + spawn_context pair
// returned by the server's list_wake_targets ws_request. Only the fields
// we need for spawning are named.
type wakeTargetPayload struct {
	AIAgentInstance struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"ai_agent_instance"`
	SpawnContext struct {
		HarnessName   string `json:"harness_name"`
		HostID        string `json:"host_id"`
		DirectoryPath string `json:"directory_path"`
		ModelProvider string `json:"model_provider"`
		ModelName     string `json:"model_name"`
	} `json:"spawn_context"`
}

// reconcileAndWakeAgents runs once after the daemon's WebSocket comes up.
// It clears leftover process/filesystem markers from any prior daemon
// life, fetches the list of agents that SHOULD be running on this host
// (status='active', not retired, position alive, WD alive), and spawns
// them serially. Spawn failures are reported as pid_status='spawn_failed'
// so the UI can surface them; they do NOT flip status, which is user
// intent.
//
// Phase 1 caveat: we don't yet "adopt" a live orphan process — any leftover
// stream subprocess, bridge file, or interpose socket from a prior daemon
// is killed/removed and we respawn from scratch. A user's mid-conversation
// claude session will resume from its on-disk .jsonl, but any in-flight
// tool call is lost. True reconnect-to-live-PID adoption is a future
// phase; see docs/daemon-agent-lifecycle.md.
func (d *Daemon) reconcileAndWakeAgents() {
	if d.daemonWS == nil {
		log.Printf("daemon: skipping agent reconciliation — no WebSocket")
		return
	}

	d.cleanupOrphanMarkers()

	targets, err := d.fetchWakeTargets()
	if err != nil {
		log.Printf("daemon: wake-target fetch failed: %v (will be picked up by a future reconcile)", err)
		return
	}
	if len(targets) == 0 {
		log.Printf("daemon: no agents to wake")
		return
	}

	log.Printf("daemon: waking %d agent(s)", len(targets))
	for i, t := range targets {
		if i > 0 {
			time.Sleep(agentWakeStagger)
		}
		d.wakeOneAgent(t)
	}
}

// cleanupOrphanMarkers sweeps /tmp for per-agent files left behind by a
// previous daemon life. In order of cleanup:
//
//   - /tmp/greenlight-stream-<id>.pid — if the named PID is still alive,
//     SIGKILL it. The stream subprocess is a detached `greenlight stream`
//     that tails the agent's on-disk transcript; a live orphan keeps
//     writing into a dead bridge file.
//   - /tmp/greenlight-bridge-<id> — stale bridge file; harmless but clutter.
//   - /tmp/greenlight-interpose-<id>.log — dev-build debug log; clutter.
//   - /tmp/gl-<hex>.sock — interpose unix socket; no listener once the
//     prior daemon died, so any surviving agent child will fail to send
//     permission requests until we respawn. Removed so the new daemon can
//     recreate cleanly.
//   - /tmp/.gl-<hex> — extracted interpose library; harmless, removed.
func (d *Daemon) cleanupOrphanMarkers() {
	dir := os.TempDir()

	// Stream PID files — kill any still-live orphan stream processes.
	if entries, err := filepath.Glob(filepath.Join(dir, "greenlight-stream-*.pid")); err == nil {
		for _, path := range entries {
			data, err := os.ReadFile(path)
			if err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
					if proc, err := os.FindProcess(pid); err == nil {
						// Signal(0) returns nil if the process exists.
						if proc.Signal(syscall.Signal(0)) == nil {
							log.Printf("daemon: killing orphan stream pid %d (%s)", pid, filepath.Base(path))
							_ = proc.Kill()
						}
					}
				}
			}
			_ = os.Remove(path)
		}
	}

	// Bridge files, interpose socks, interpose libs, interpose logs.
	for _, pattern := range []string{
		"greenlight-bridge-*",
		"greenlight-interpose-*.log",
		"gl-*.sock",
		".gl-*",
	} {
		if entries, err := filepath.Glob(filepath.Join(dir, pattern)); err == nil {
			for _, path := range entries {
				_ = os.Remove(path)
			}
		}
	}
}

// fetchWakeTargets asks the server for the agents that should be running
// on this host. Blocks on the WS; returns an error if the WS isn't
// connected or the response is an error envelope.
func (d *Daemon) fetchWakeTargets() ([]wakeTargetPayload, error) {
	if !d.daemonWS.IsConnected() {
		return nil, fmt.Errorf("daemon WS not connected")
	}
	data, _ := json.Marshal(map[string]string{"host_id": d.hostID})
	resp, err := d.daemonWS.SendWSRequest(generateUUID(), "list_wake_targets", data)
	if err != nil {
		return nil, err
	}
	var env struct {
		Type        string              `json:"type"`
		Error       string              `json:"error"`
		WakeTargets []wakeTargetPayload `json:"wake_targets"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return nil, fmt.Errorf("parse wake_targets: %w", err)
	}
	if env.Error != "" {
		return nil, fmt.Errorf("server: %s", env.Error)
	}
	return env.WakeTargets, nil
}

// wakeOneAgent spawns a single instance from a wake-target payload. On
// failure it reports pid_status='spawn_failed' so the server records the
// attempt (via last_spawn_attempt_at) and the UI can show "last tried
// X min ago". We never mutate status — that's user intent and their
// problem to resolve once we've told them what went wrong.
func (d *Daemon) wakeOneAgent(t wakeTargetPayload) {
	id := t.AIAgentInstance.ID
	if id == "" {
		log.Printf("daemon: wake target missing id, skipping")
		return
	}
	// Defensive: the list query filters by host, but double-check to
	// avoid spawning for a different host if the response is malformed.
	if t.SpawnContext.HostID != "" && d.hostID != "" && t.SpawnContext.HostID != d.hostID {
		log.Printf("daemon: wake %s: spawn_context host %s doesn't match this daemon %s, skipping",
			id, t.SpawnContext.HostID, d.hostID)
		return
	}

	d.mu.RLock()
	_, alreadyRunning := d.instances[id]
	d.mu.RUnlock()
	if alreadyRunning {
		log.Printf("daemon: wake %s: already running locally, skipping", id)
		return
	}

	if _, err := d.spawnAgentInstance(
		id,
		t.AIAgentInstance.Name,
		t.SpawnContext.HarnessName,
		t.SpawnContext.DirectoryPath,
		t.SpawnContext.ModelProvider,
		t.SpawnContext.ModelName,
	); err != nil {
		log.Printf("daemon: wake %s: spawn failed: %v", id, err)
		d.reportPIDStatus(id, "spawn_failed")
	}
}
