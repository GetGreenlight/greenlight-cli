//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// AgentSetup holds the results of building the agent command and session IDs.
type AgentSetup struct {
	Command        string
	Args           []string
	AgentSessionID string // deterministic session ID for transcript path derivation
	RelayID        string // relay ID for server communication
}

// InterposeSetup holds interpose library state for the caller to manage cleanup.
type InterposeSetup struct {
	LibPath      string
	LibExtracted bool
	SockPath     string
	SockCleanup  func()
	Relay        *interposeRelay // per-session relay reference; caller must call SetRelay
}

// buildAgentCommand constructs the agent binary command, flags, session IDs,
// and relay ID. This is the shared core between direct and daemon modes.
// If ticket is non-nil, agents that use --append-system-prompt get a line
// pointing at the ticket URL. shims is the active command-shim set (resolved
// at session start); when non-empty the system prompt names those commands as
// pre-authenticated (issue #198).
func buildAgentCommand(agent, resume string, ticket *TicketRef, shims []resolvedShim) (*AgentSetup, error) {
	command := agentBinary(agent)
	var cmdArgs []string

	// Generate a session ID for agents that support it. This lets us
	// derive the transcript path deterministically, avoiding the bug where
	// two concurrent sessions in the same CWD pick up the same transcript.
	var agentSessionID string
	if resume != "" {
		// Resuming: the conversation ID IS the agent session ID
		agentSessionID = resume
	} else if agent == "claude" || agent == "copilot" {
		agentSessionID = generateUUID()
	}

	// Agent-specific resume handling
	if resume != "" {
		switch agent {
		case "codex":
			// Codex uses a subcommand: codex resume <id>
			cmdArgs = append(cmdArgs, "resume", resume)
		case "pi":
			return nil, fmt.Errorf("--resume is not supported for pi; use /resume to pick a session once pi starts")
		default:
			cmdArgs = append(cmdArgs, "--resume", resume)
		}
	}

	// Agent-specific flags
	switch agent {
	case "copilot":
		cmdArgs = append(cmdArgs, "--allow-all")
		// Set session ID for new sessions (--resume also works for new Copilot sessions)
		if resume == "" && agentSessionID != "" {
			cmdArgs = append(cmdArgs, "--resume", agentSessionID)
		}
	case "cursor":
		cmdArgs = append(cmdArgs, "--yolo")
	case "gemini":
		cmdArgs = append(cmdArgs, "--yolo")
	case "codex":
		cmdArgs = append(cmdArgs, "--dangerously-bypass-approvals-and-sandbox")
	case "claude":
		cmdArgs = append(cmdArgs, "--dangerously-skip-permissions", "--append-system-prompt", greenlightSystemPrompt(ticket, shims))
		if resume == "" && agentSessionID != "" {
			cmdArgs = append(cmdArgs, "--session-id", agentSessionID)
		}
	case "pi":
		cmdArgs = append(cmdArgs, "--append-system-prompt", greenlightSystemPrompt(ticket, shims))
		if agentSessionID != "" {
			sessPath := piSessionPath(agentSessionID, "")
			if sessPath != "" {
				cmdArgs = append(cmdArgs, "--session", sessPath)
			}
		}
	}

	// Generate relay ID (reuse for resumed conversations)
	var relayID string
	if resume != "" {
		relayID = lookupRelayID(resume)
	}
	if relayID == "" {
		relayID = generateUUID()
	}

	return &AgentSetup{
		Command:        command,
		Args:           cmdArgs,
		AgentSessionID: agentSessionID,
		RelayID:        relayID,
	}, nil
}

// resolveDeviceAndProject resolves device ID and project name from explicit
// values, falling back to env vars and config file.
func resolveDeviceAndProject(deviceID, project, cwd string) (string, string, error) {
	devID := deviceID
	if devID == "" {
		devID = os.Getenv("GREENLIGHT_DEVICE_ID")
	}
	if devID == "" {
		devID = readConfigValue("device_id")
	}
	if devID == "" {
		return "", "", fmt.Errorf("device ID is required (use --device-id, GREENLIGHT_DEVICE_ID, or set device_id in ~/.greenlight/config)")
	}

	proj := project
	if proj == "" {
		proj = os.Getenv("GREENLIGHT_PROJECT")
	}
	if proj == "" {
		proj = readConfigValue("project")
	}
	if proj == "" && cwd != "" {
		proj = filepath.Base(cwd)
	}
	if proj == "" {
		return "", "", fmt.Errorf("project name is required (use --project)")
	}

	return devID, proj, nil
}

// installAgentFiles installs agent-specific instruction files and hooks.
func installAgentFiles(agent, relayID, cwd string, ticket *TicketRef, shims []resolvedShim) {
	if agent == "gemini" || agent == "copilot" || agent == "cursor" || agent == "codex" {
		if err := installGreenlightInstructions(agent, relayID, cwd, ticket, shims); err != nil {
			log.Printf("Warning: failed to install agent instructions: %v", err)
		}
	}
	installHooks(agent, cwd)
}

// buildExportEnvs returns the GREENLIGHT_* environment variables for the child process.
// ticketJSON, when non-empty, is the session's in-scope ticket (marshaled
// TicketRef) so the agent can run `greenlight ticket start|submit` without an id.
func buildExportEnvs(devID, relayID, proj, bridgePath, agent, ticketJSON string) map[string]string {
	envs := map[string]string{
		"GREENLIGHT_DEVICE_ID":  devID,
		"GREENLIGHT_SESSION_ID": relayID,
		"GREENLIGHT_PROJECT":    proj,
		"GREENLIGHT_BRIDGE":     bridgePath,
		"GREENLIGHT_AGENT":      agent,
	}
	if ticketJSON != "" {
		envs["GREENLIGHT_TICKET_JSON"] = ticketJSON
	}
	return envs
}

// setupInterpose configures library interposition (DYLD_INSERT_LIBRARIES or
// LD_PRELOAD), starts the interpose socket, and resolves script commands.
// It modifies exportEnvs in place and may modify command/args for dyld.
// Returns the resolved command, args, and interpose state for cleanup.
func setupInterpose(agent, command string, args []string, relayID string, cwd string, exportEnvs map[string]string) (string, []string, *InterposeSetup, error) {
	setup := &InterposeSetup{}
	// Test-only escape hatch: skip library injection entirely. The agent
	// runs without permission gating. Used by integration tests where the
	// macOS dyld entitlement re-sign step (which requires codesign) is
	// unreliable on CI runners.
	if os.Getenv("GREENLIGHT_DISABLE_INTERPOSE") == "1" {
		log.Printf("Interpose: disabled via GREENLIGHT_DISABLE_INTERPOSE")
		return command, args, setup, nil
	}
	setup.LibPath, setup.LibExtracted = findInterposeLib()
	if setup.LibPath == "" {
		return "", nil, nil, fmt.Errorf("interpose library not found")
	}

	if version == "" || version == "dev" {
		logPath := filepath.Join(os.TempDir(), "greenlight-interpose-"+relayID+".log")
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
	sockPath, sockCleanup, ir, err := startInterposeSock(relayID, agentServerName(agent))
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
