//go:build darwin || linux

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// installGreenlightInstructions creates an agent-specific instruction file
// that teaches the agent how to interpret [GREENLIGHT] permission denial messages.
// If ticket is non-nil, a one-line note pointing at the ticket URL is appended.
func installGreenlightInstructions(agent, relayID, cwd string, ticket *TicketRef) error {
	var instrPath string
	switch agent {
	case "gemini":
		instrPath = filepath.Join(cwd, "GEMINI.md")
	case "copilot":
		dir := filepath.Join(cwd, ".github")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		instrPath = filepath.Join(dir, "copilot-instructions.md")
	case "cursor":
		dir := filepath.Join(cwd, ".cursor", "rules")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		instrPath = filepath.Join(dir, "greenlight.mdc")
	case "codex":
		instrPath = filepath.Join(cwd, "AGENTS.md")
	default:
		return nil
	}

	// Don't overwrite an existing file that the user created
	if _, err := os.Stat(instrPath); err == nil {
		existing, err := os.ReadFile(instrPath)
		if err == nil && !strings.Contains(string(existing), "[GREENLIGHT]") {
			log.Printf("Skipping %s — user file exists", instrPath)
			return nil
		}
	}

	content := "<!-- Greenlight -->\n" + greenlightSystemPrompt(ticket) + "\n"
	if err := os.WriteFile(instrPath, []byte(content), 0644); err != nil {
		return err
	}
	log.Printf("Installed greenlight instructions in %s", instrPath)
	return nil
}

// removeGreenlightInstructions removes the instruction file only if it was
// created by greenlight (contains our marker).
func removeGreenlightInstructions(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.Contains(string(data), "<!-- Greenlight -->") {
		if err := os.Remove(path); err == nil {
			log.Printf("Removed greenlight instructions %s", path)
		}
	}
}

const (
	hookSettingsFile = ".claude/settings.local.json"
	// AskUserQuestion and ExitPlanMode both always request permission
	// (their checkPermissions returns "ask"), so we gate them via the
	// PermissionRequest hook event — the one Claude Code documents as
	// "run before permission prompt". This lets the hook supply the
	// decision (and, for AskUserQuestion, the answers via updatedInput)
	// so Claude never shows its own terminal menu. PreToolUse does not
	// work here: the tool's own checkPermissions would still fire and
	// re-prompt.
	hookEventType = "PermissionRequest"
	hookMatcher   = "AskUserQuestion|ExitPlanMode"
	hookCommand   = "greenlight hook"
)

// installHooks appends AskUserQuestion and ExitPlanMode PermissionRequest hook
// entries to .claude/settings.local.json in cwd (creating the file if needed).
// Only applies for the claude agent. Idempotent — won't add duplicate entries.
func installHooks(agent, cwd string) {
	if agent != "claude" {
		return
	}
	path := filepath.Join(cwd, hookSettingsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Printf("installHooks: cannot create dir: %v", err)
		return
	}

	// Parse existing file or start fresh.
	raw := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &raw)
	}

	hooks, _ := raw["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
	}

	entries, _ := hooks[hookEventType].([]interface{})
	if hookEntryExists(entries, hookMatcher, hookCommand) {
		return // already installed
	}

	entries = append(entries, map[string]interface{}{
		"matcher": hookMatcher,
		"hooks": []interface{}{
			map[string]interface{}{
				"type":    "command",
				"command": hookCommand,
			},
		},
	})
	hooks[hookEventType] = entries
	raw["hooks"] = hooks

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		log.Printf("installHooks: marshal error: %v", err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		log.Printf("installHooks: write error: %v", err)
		return
	}
	log.Printf("Installed greenlight hooks in %s", path)
}

// removeHooks removes the greenlight hook entries from .claude/settings.local.json.
// Only applies for the claude agent.
func removeHooks(agent, cwd string) {
	if agent != "claude" {
		return
	}
	path := filepath.Join(cwd, hookSettingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	raw := map[string]interface{}{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	hooks, _ := raw["hooks"].(map[string]interface{})
	if hooks == nil {
		return
	}

	entries, _ := hooks[hookEventType].([]interface{})
	filtered := entries[:0]
	for _, e := range entries {
		if !isGreenlightHookEntry(e, hookMatcher, hookCommand) {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == len(entries) {
		return // nothing changed
	}

	if len(filtered) == 0 {
		delete(hooks, hookEventType)
	} else {
		hooks[hookEventType] = filtered
	}
	if len(hooks) == 0 {
		delete(raw, "hooks")
	} else {
		raw["hooks"] = hooks
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, append(out, '\n'), 0644); err != nil {
		log.Printf("removeHooks: write error: %v", err)
		return
	}
	log.Printf("Removed greenlight hooks from %s", path)
}

// hookEntryExists returns true if entries already contains a hook entry with
// the given matcher and command.
func hookEntryExists(entries []interface{}, matcher, command string) bool {
	for _, e := range entries {
		if isGreenlightHookEntry(e, matcher, command) {
			return true
		}
	}
	return false
}

// isGreenlightHookEntry returns true if e is the hook entry we installed.
func isGreenlightHookEntry(e interface{}, matcher, command string) bool {
	entry, _ := e.(map[string]interface{})
	if entry == nil {
		return false
	}
	if entry["matcher"] != matcher {
		return false
	}
	hs, _ := entry["hooks"].([]interface{})
	for _, h := range hs {
		hm, _ := h.(map[string]interface{})
		if hm != nil && hm["command"] == command {
			return true
		}
	}
	return false
}
