//go:build darwin || linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// knownAgents lists the valid agent runtime values.
var knownAgents = map[string]bool{
	"claude":  true,
	"copilot": true,
	"cursor":  true,
	"codex":   true,
	"pi":      true,
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
	case "cursor":
		return "agent"
	case "codex":
		return "codex"
	case "pi":
		return "pi"
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
	case "cursor":
		return "cursor"
	case "codex":
		return "codex"
	case "pi":
		return "pi"
	default:
		return "claude-code"
	}
}

// greenlightSystemPrompt is appended to the agent's system prompt to teach it
// how to interpret permission denials from the Greenlight interpose library.
const greenlightSystemPrompt = `Tool calls are managed by a permission system called Greenlight. ` +
	`If a command exits with code 126, or a file operation fails with "Permission denied", ` +
	`the user has explicitly denied this action. ` +
	`Do not retry the same action. Try a different approach or ask the user what they'd like instead. ` +
	`If a command exits with code 127, or a file operation fails with "Operation not permitted", ` +
	`the user wants you to stop. Do not continue with any further tool calls. ` +
	`Explain what you were doing and wait for new instructions.`

// deriveTranscriptPath constructs the transcript file path for the given agent.
// For Claude it finds the newest .jsonl in the project dir; for Copilot it
// finds the newest session dir. Returns "" if it can't be determined.
func deriveTranscriptPath(agent, sessionID string) string {
	switch agent {
	case "claude":
		if sessionID != "" {
			return deriveClaudeTranscriptPathByID(sessionID)
		}
		return deriveClaudeTranscriptPath()
	case "copilot":
		if sessionID != "" {
			return deriveCopilotTranscriptPathByID(sessionID)
		}
		return deriveCopilotTranscriptPath()
	case "gemini":
		return deriveGeminiTranscriptPath()
	case "cursor":
		return deriveCursorTranscriptPath()
	case "codex":
		if sessionID != "" {
			return deriveCodexTranscriptBySentinel(sessionID)
		}
		return deriveCodexTranscriptPath()
	case "pi":
		if sessionID != "" {
			return piSessionPath(sessionID)
		}
		return derivePiTranscriptPath()
	default:
		return ""
	}
}

// deriveClaudeTranscriptPathByID returns the transcript path for a known session ID.
// The file may not exist yet (the caller polls until it appears).
func deriveClaudeTranscriptPathByID(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	projHash := strings.ReplaceAll(cwd, "/", "-")
	return filepath.Join(home, ".claude", "projects", projHash, sessionID+".jsonl")
}

func deriveClaudeTranscriptPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Claude project dir is CWD with "/" replaced by "-"
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	projHash := strings.ReplaceAll(cwd, "/", "-")
	projDir := filepath.Join(home, ".claude", "projects", projHash)
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
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
		return filepath.Join(projDir, newest)
	}
	return ""
}

// deriveCopilotTranscriptPathByID returns the transcript path for a known session ID.
func deriveCopilotTranscriptPathByID(sessionID string) string {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".copilot")
	}
	return filepath.Join(home, "session-state", sessionID, "events.jsonl")
}

func deriveCopilotTranscriptPath() string {
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

func deriveGeminiTranscriptPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Gemini uses the CWD basename as the project name
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	projName := filepath.Base(cwd)
	chatsDir := filepath.Join(home, ".gemini", "tmp", projName, "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
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
		return filepath.Join(chatsDir, newest)
	}
	return ""
}

func deriveCursorTranscriptPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Cursor project dir uses CWD with "/" replaced by "-"
	projHash := strings.ReplaceAll(cwd, "/", "-")
	// Strip leading "-" (from absolute paths like /Users/...)
	projHash = strings.TrimLeft(projHash, "-")
	transcriptsDir := filepath.Join(home, ".cursor", "projects", projHash, "agent-transcripts")
	entries, err := os.ReadDir(transcriptsDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
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
		return filepath.Join(transcriptsDir, newest)
	}
	return ""
}

// deriveCodexTranscriptBySentinel scans recent Codex transcript files for
// a greenlight-relay sentinel embedded in the AGENTS.md content that Codex
// serializes into the transcript at session start.
func deriveCodexTranscriptBySentinel(relayID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	sentinel := "greenlight-relay:" + relayID
	sessionsDir := filepath.Join(home, ".codex", "sessions")

	// Collect candidates sorted newest first to find our session quickly.
	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		candidates = append(candidates, candidate{path, info.ModTime()})
		return nil
	})
	// Sort newest first
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].mtime.After(candidates[j].mtime)
	})

	// Only check the 5 newest files — the sentinel should be in a very recent one.
	limit := 5
	if len(candidates) < limit {
		limit = len(candidates)
	}
	for _, c := range candidates[:limit] {
		f, err := os.Open(c.path)
		if err != nil {
			continue
		}
		found := false
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		// The sentinel appears in the first few lines (AGENTS.md content).
		for i := 0; i < 20 && scanner.Scan(); i++ {
			if strings.Contains(scanner.Text(), sentinel) {
				found = true
				break
			}
		}
		f.Close()
		if found {
			return c.path
		}
	}
	return ""
}

func deriveCodexTranscriptPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Codex stores sessions in date-nested dirs:
	// ~/.codex/sessions/YYYY/MM/DD/rollout-YYYY-MM-DDTHH-MM-SS-UUID.jsonl
	// Walk the tree to find the newest .jsonl file.
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	var newest string
	var newestTime time.Time

	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = path
		}
		return nil
	})
	return newest
}

// piSessionPath returns the transcript file path for a Pi session ID,
// following Pi's convention: $PI_CODING_AGENT_DIR/sessions/<normalized-cwd>/<sessionID>.jsonl
// PI_CODING_AGENT_DIR defaults to ~/.pi/agent.
func piSessionPath(sessionID string) string {
	baseDir := os.Getenv("PI_CODING_AGENT_DIR")
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		baseDir = filepath.Join(home, ".pi", "agent")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Pi encodes CWD as: strip leading /, replace /\: with -, wrap in --
	safePath := strings.TrimLeft(cwd, "/\\")
	safePath = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safePath)
	safePath = "--" + safePath + "--"
	return filepath.Join(baseDir, "sessions", safePath, sessionID+".jsonl")
}

func derivePiTranscriptPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Pi encodes CWD as: strip leading /, replace /\: with -, wrap in --
	safePath := strings.TrimLeft(cwd, "/\\")
	safePath = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safePath)
	safePath = "--" + safePath + "--"
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", safePath)
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
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
		return filepath.Join(sessDir, newest)
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
		fmt.Fprintf(os.Stderr, "Supported agents: claude, codex, copilot, cursor, pi\n")
		os.Exit(0)
	}

	name := args[0]
	if !knownAgents[name] {
		fmt.Fprintf(os.Stderr, "greenlight: unknown agent %q (supported: claude, codex, copilot, cursor, pi)\n", name)
		os.Exit(1)
	}

	if err := writeConfigValue("agent", name); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Default agent set to %s\n", name)
}
