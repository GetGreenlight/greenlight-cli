//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// knownAgents lists the valid agent runtime values.
var knownAgents = map[string]bool{
	"claude": true,
	"cursor": true,
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
	case "cursor":
		return "agent"
	default:
		return "claude"
	}
}

// agentServerName returns the agent identifier sent to the server.
func agentServerName(agent string) string {
	switch agent {
	case "cursor":
		return "cursor"
	default:
		return "claude-code"
	}
}

// deriveTranscriptPath constructs the transcript file path for agents that
// don't include it in hook input. Returns "" if it can't be determined.
func deriveTranscriptPath(agent, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Convert CWD to slug: "/Users/dave/project" → "Users-dave-project"
	slug := strings.TrimPrefix(cwd, "/")
	slug = strings.ReplaceAll(slug, string(filepath.Separator), "-")

	switch agent {
	case "cursor":
		return filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", sessionID+".jsonl")
	default:
		return ""
	}
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
		fmt.Fprintf(os.Stderr, "Supported agents: claude, cursor\n")
		os.Exit(0)
	}

	name := args[0]
	if !knownAgents[name] {
		fmt.Fprintf(os.Stderr, "greenlight: unknown agent %q (supported: claude, cursor)\n", name)
		os.Exit(1)
	}

	if err := writeConfigValue("agent", name); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Default agent set to %s\n", name)
}
