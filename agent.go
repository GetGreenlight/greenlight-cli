//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// knownAgents lists the valid agent runtime values.
var knownAgents = map[string]bool{
	"claude":  true,
	"gemini":  true,
	"copilot": true,
}

const defaultAgent = "claude"

// resolveAgent resolves the agent runtime from flag > env > config > default.
func resolveAgent(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("GREENLIGHT_AGENT"); v != "" {
		return v
	}
	if v := readConfigValue("agent"); v != "" {
		return v
	}
	return defaultAgent
}

// agentBinary returns the CLI binary name for the given agent runtime.
func agentBinary(agent string) string {
	switch agent {
	case "gemini":
		return "gemini"
	case "copilot":
		return "copilot"
	default:
		return "claude"
	}
}

// agentServerName returns the agent identifier sent to the server.
func agentServerName(agent string) string {
	switch agent {
	case "gemini":
		return "gemini"
	case "copilot":
		return "copilot"
	default:
		return "claude-code"
	}
}

// agentSettingsPath returns the path to the agent's local settings file
// relative to the project directory.
func agentSettingsPath(agent string) string {
	switch agent {
	case "gemini":
		return filepath.Join(".gemini", "settings.json")
	case "copilot":
		return filepath.Join(".github", "hooks", "greenlight.json")
	default:
		return filepath.Join(".claude", "settings.local.json")
	}
}

// agentSettingsDir returns the settings directory name for the agent.
func agentSettingsDir(agent string) string {
	switch agent {
	case "gemini":
		return ".gemini"
	case "copilot":
		return filepath.Join(".github", "hooks")
	default:
		return ".claude"
	}
}

// agentHookEvents returns the hook event names to register for the agent.
func agentHookEvents(agent string) []string {
	switch agent {
	case "gemini":
		return []string{"SessionStart", "BeforeTool", "Notification"}
	case "copilot":
		return []string{"sessionStart", "preToolUse"}
	default:
		return []string{"SessionStart", "PermissionRequest"}
	}
}

// agentOldHookEvents returns hook events that should be cleaned up for the agent.
func agentOldHookEvents(agent string) []string {
	switch agent {
	case "gemini":
		return nil
	case "copilot":
		return nil
	default:
		return []string{"UserPromptSubmit"}
	}
}

// deriveTranscriptPath constructs the transcript file path for agents that
// don't include it in hook input. Returns "" if it can't be determined.
func deriveTranscriptPath(agent, sessionID string) string {
	if agent != "copilot" {
		return ""
	}
	// Copilot stores sessions at $COPILOT_HOME/session-state/{id}/events.jsonl
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".copilot")
	}
	stateDir := filepath.Join(home, "session-state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(stateDir, newest, "events.jsonl")
	}
	return ""
}

func runAgent(args []string) {
	if len(args) == 0 {
		// Print current agent
		agent := resolveAgent("")
		fmt.Fprintf(os.Stderr, "%s\n", agent)
		return
	}

	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight agent [name]\n\n")
		fmt.Fprintf(os.Stderr, "Without arguments, prints the current default agent.\n")
		fmt.Fprintf(os.Stderr, "With a name, sets the default agent in ~/.greenlight/config.\n\n")
		fmt.Fprintf(os.Stderr, "Supported agents: claude, copilot, gemini\n")
		os.Exit(0)
	}

	name := args[0]
	if !knownAgents[name] {
		fmt.Fprintf(os.Stderr, "greenlight: unknown agent %q (supported: claude, copilot, gemini)\n", name)
		os.Exit(1)
	}

	if err := writeConfigValue("agent", name); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Default agent set to %s\n", name)
}
