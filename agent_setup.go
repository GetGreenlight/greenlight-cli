//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// AgentSetup holds the results of building the agent command, the
// agent-internal session ID (used to locate the agent's transcript
// JSONL on disk), and the ai_agent_instance_id used for server routing.
type AgentSetup struct {
	Command           string
	Args              []string
	AgentSessionID    string // deterministic agent-internal session ID for transcript path derivation
	AIAgentInstanceID string // ai_agent_instance_id for server communication
}

// InterposeSetup holds interpose library state for the caller to manage cleanup.
type InterposeSetup struct {
	LibPath      string
	LibExtracted bool
	SockPath     string
	SockCleanup  func()
	Relay        *interposeRelay // per-instance relay reference; caller must call SetRelay
}

// buildAgentCommand constructs the agent binary command, flags, and IDs.
// Every invocation produces a fresh ai_agent_instance_id — resume is gone.
func buildAgentCommand(agent string) (*AgentSetup, error) {
	command := agentBinary(agent)
	var cmdArgs []string

	// Every connect creates a fresh ai_agent_instance_id.
	aiAgentInstanceID := generateUUID()

	// Generate an agent-internal session ID for agents that support it. This
	// lets us derive the transcript path deterministically, avoiding the bug
	// where two concurrent sessions in the same CWD pick up the same transcript.
	var agentSessionID string
	if agent == "claude" || agent == "copilot" || agent == "pi" {
		agentSessionID = generateUUID()
	}

	// Agent-specific flags
	switch agent {
	case "copilot":
		cmdArgs = append(cmdArgs, "--allow-all")
		if agentSessionID != "" {
			cmdArgs = append(cmdArgs, "--resume", agentSessionID)
		}
	case "cursor":
		cmdArgs = append(cmdArgs, "--yolo")
	case "gemini":
		cmdArgs = append(cmdArgs, "--yolo")
	case "codex":
		cmdArgs = append(cmdArgs, "--dangerously-bypass-approvals-and-sandbox")
	case "claude":
		cmdArgs = append(cmdArgs, "--dangerously-skip-permissions", "--append-system-prompt", greenlightSystemPrompt)
		if agentSessionID != "" {
			cmdArgs = append(cmdArgs, "--session-id", agentSessionID)
		}
	case "pi":
		cmdArgs = append(cmdArgs, "--append-system-prompt", greenlightSystemPrompt)
		if agentSessionID != "" {
			sessPath := piSessionPath(agentSessionID, "")
			if sessPath != "" {
				cmdArgs = append(cmdArgs, "--session", sessPath)
			}
		}
	}

	// For Codex, use ai_agent_instance_id as the sentinel for transcript matching.
	if agent == "codex" {
		agentSessionID = aiAgentInstanceID
	}

	return &AgentSetup{
		Command:           command,
		Args:              cmdArgs,
		AgentSessionID:    agentSessionID,
		AIAgentInstanceID: aiAgentInstanceID,
	}, nil
}

// installAgentFiles installs agent-specific instruction files for agents that use them.
func installAgentFiles(agent, aiAgentInstanceID string) {
	if agent == "gemini" || agent == "copilot" || agent == "cursor" || agent == "codex" {
		if err := installGreenlightInstructions(agent, aiAgentInstanceID); err != nil {
			log.Printf("Warning: failed to install agent instructions: %v", err)
		}
	}
}

// buildExportEnvs returns the GREENLIGHT_* environment variables for the child
// process, plus any agent-specific model env vars when modelName is non-empty.
func buildExportEnvs(devID, aiAgentInstanceID, proj, bridgePath, agent, modelName string) map[string]string {
	envs := map[string]string{
		"GREENLIGHT_DEVICE_ID":         devID,
		"GREENLIGHT_AGENT_INSTANCE_ID": aiAgentInstanceID,
		"GREENLIGHT_PROJECT":           proj,
		"GREENLIGHT_BRIDGE":            bridgePath,
		"GREENLIGHT_AGENT":             agent,
	}
	// Pin the child to the org's chosen model. Every agent binary uses a
	// different knob; see each case below. If modelName is empty we skip —
	// the agent falls back to its own default.
	if modelName != "" {
		switch agent {
		case "claude":
			// Anthropic CLI reads ANTHROPIC_MODEL for the primary model.
			envs["ANTHROPIC_MODEL"] = modelName
		case "codex":
			// codex reads OPENAI_MODEL.
			envs["OPENAI_MODEL"] = modelName
		}
		// copilot, cursor, gemini, pi: no documented env var for model
		// selection — model is configured inside the tool's own UI. Leave
		// modelName informational; the agent will use its own default.
	}
	return envs
}

// setupInterpose configures library interposition (DYLD_INSERT_LIBRARIES or
// LD_PRELOAD), starts the interpose socket, and resolves script commands.
// It modifies exportEnvs in place and may modify command/args for dyld.
// Returns the resolved command, args, and interpose state for cleanup.
func setupInterpose(agent, command string, args []string, aiAgentInstanceID string, cwd string, exportEnvs map[string]string) (string, []string, *InterposeSetup, error) {
	setup := &InterposeSetup{}
	setup.LibPath, setup.LibExtracted = findInterposeLib()
	if setup.LibPath == "" {
		return "", nil, nil, fmt.Errorf("interpose library not found")
	}

	if version == "" || version == "dev" {
		logPath := filepath.Join(os.TempDir(), "greenlight-interpose-"+aiAgentInstanceID+".log")
		exportEnvs["GREENLIGHT_INTERPOSE_LOG"] = logPath
	}

	if runtime.GOOS == "darwin" {
		entitlementTarget := command
		if agent == "cursor" {
			if nodeBin := resolveCursorNodeBin(command); nodeBin != "" {
				entitlementTarget = nodeBin
			}
		}
		if err := ensureDyldEntitlement(entitlementTarget); err != nil {
			return "", nil, nil, fmt.Errorf("cannot ensure dyld entitlement for %s: %w", entitlementTarget, err)
		}
		exportEnvs["DYLD_INSERT_LIBRARIES"] = setup.LibPath
		ensureAgentHelpers(agent)
	} else {
		exportEnvs["LD_PRELOAD"] = setup.LibPath
	}

	// Set project dir for seccomp path classification (Linux)
	if cwd != "" {
		seccompProjectDir = cwd
	}

	// Start permission socket for interpose library
	sockPath, sockCleanup, ir, err := startInterposeSock(aiAgentInstanceID, agentServerName(agent))
	if err != nil {
		return "", nil, nil, err
	}
	setup.SockPath = sockPath
	setup.SockCleanup = sockCleanup
	setup.Relay = ir
	exportEnvs["GREENLIGHT_INTERPOSE_SOCK"] = sockPath
	log.Printf("Interpose socket: %s", sockPath)
	log.Printf("Interpose library: %s", setup.LibPath)

	// If we're injecting a dylib and the command is a script, launch the
	// interpreter directly to avoid /usr/bin/env stripping DYLD_INSERT_LIBRARIES.
	if exportEnvs["DYLD_INSERT_LIBRARIES"] != "" {
		if agent == "cursor" {
			command, args, err = resolveCursorCommand(command, args)
			if err != nil {
				return "", nil, nil, err
			}
		} else {
			command, args = resolveScriptCommand(command, args)
		}
	}

	return command, args, setup, nil
}
