//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installHooks upserts the agent's settings file in the current working
// directory to register the greenlight hook for the appropriate events.
func installHooks(agent string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	hookCmd := exe + " hook"

	// Copilot uses a completely different hook file format
	if agent == "copilot" {
		return installCopilotHooks(exe)
	}

	dir := agentSettingsDir(agent)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s dir: %w", dir, err)
	}

	settingsPath := agentSettingsPath(agent)

	// Read existing settings or start fresh
	var settings map[string]interface{}
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	} else {
		settings = make(map[string]interface{})
	}

	// Build the hook entry
	hookEntry := []interface{}{
		map[string]interface{}{
			"matcher": "",
			"hooks": []interface{}{
				map[string]interface{}{
					"type":    "command",
					"command": hookCmd,
				},
			},
		},
	}

	// Get or create the hooks map
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
	}

	// Upsert our hook entries, replacing any existing greenlight hooks
	// but preserving non-greenlight hooks on the same event
	for _, event := range agentHookEvents(agent) {
		hooks[event] = upsertGreenlightHook(hooks[event], hookEntry, hookCmd)
	}

	// Remove old greenlight hooks from events we no longer use
	for _, oldEvent := range agentOldHookEvents(agent) {
		if existing, ok := hooks[oldEvent]; ok {
			cleaned := removeGreenlightHooks(existing)
			if len(cleaned) == 0 {
				delete(hooks, oldEvent)
			} else {
				hooks[oldEvent] = cleaned
			}
		}
	}

	settings["hooks"] = hooks

	if agent == "gemini" {
		// Enable plan mode so enter_plan_mode/exit_plan_mode tools are available
		exp, _ := settings["experimental"].(map[string]interface{})
		if exp == nil {
			exp = map[string]interface{}{}
		}
		exp["plan"] = true
		settings["experimental"] = exp

		// Enable folder trust so workspace-level policies are loaded
		sec, _ := settings["security"].(map[string]interface{})
		if sec == nil {
			sec = map[string]interface{}{}
		}
		ft, _ := sec["folderTrust"].(map[string]interface{})
		if ft == nil {
			ft = map[string]interface{}{}
		}
		ft["enabled"] = true
		sec["folderTrust"] = ft
		settings["security"] = sec
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0644); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}

	log.Printf("Installed hooks in %s", settingsPath)

	// Gemini: install policy to auto-approve tools so the CLI doesn't
	// double-prompt (greenlight's BeforeTool hook handles permissions).
	if agent == "gemini" {
		if err := installGeminiPolicy(); err != nil {
			log.Printf("Warning: failed to install gemini policy: %v", err)
		}
	}

	return nil
}

// installCopilotHooks creates the .github/hooks/greenlight.json hook file
// for copilot-cli. Copilot loads hooks from the git root's .github/hooks/
// directory, so we detect the git root and install there.
// Existing non-greenlight hooks in the file are preserved.
func installCopilotHooks(exe string) error {
	// Copilot looks for hooks at the git root, not CWD
	root := gitRoot()
	dir := filepath.Join(root, agentSettingsDir("copilot"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s dir: %w", dir, err)
	}

	settingsPath := filepath.Join(root, agentSettingsPath("copilot"))

	// Read existing file or start fresh
	var config map[string]interface{}
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	} else {
		config = make(map[string]interface{})
	}

	config["version"] = 1

	hooks, _ := config["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = make(map[string]interface{})
	}

	for _, event := range agentHookEvents("copilot") {
		hookCmd := exe + " hook " + event
		entry := map[string]interface{}{
			"type": "command",
			"bash": hookCmd,
		}
		// preToolUse needs a long timeout for phone approval long-poll
		if event == "preToolUse" {
			entry["timeoutSec"] = 600
		}

		hooks[event] = upsertCopilotHook(hooks[event], entry)
	}

	config["hooks"] = hooks

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0644); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}

	log.Printf("Installed copilot hooks in %s", settingsPath)
	return nil
}

// upsertCopilotHook replaces or appends a greenlight hook in a copilot hook array.
func upsertCopilotHook(existing interface{}, newEntry map[string]interface{}) []interface{} {
	arr, ok := existing.([]interface{})
	if !ok || len(arr) == 0 {
		return []interface{}{newEntry}
	}

	found := false
	result := make([]interface{}, 0, len(arr))
	for _, entry := range arr {
		m, ok := entry.(map[string]interface{})
		if !ok {
			result = append(result, entry)
			continue
		}
		if isCopilotGreenlightHook(m) {
			if !found {
				result = append(result, newEntry)
				found = true
			}
		} else {
			result = append(result, entry)
		}
	}
	if !found {
		result = append(result, newEntry)
	}
	return result
}

// isCopilotGreenlightHook checks if a copilot hook entry belongs to greenlight.
func isCopilotGreenlightHook(entry map[string]interface{}) bool {
	bash, _ := entry["bash"].(string)
	if bash == "" {
		return false
	}
	bin := bash
	if i := strings.IndexByte(bash, ' '); i >= 0 {
		bin = bash[:i]
	}
	return strings.Contains(filepath.Base(bin), "greenlight")
}

// gitRoot returns the root of the current git repository, or "." if not in one.
func gitRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "."
	}
	return strings.TrimSpace(string(out))
}

// installGeminiPolicy creates a policy file that auto-approves all tools,
// letting the BeforeTool hook handle permissions via greenlight instead of
// the gemini CLI's built-in prompt.
func installGeminiPolicy() error {
	policyDir := filepath.Join(".gemini", "policies")
	if err := os.MkdirAll(policyDir, 0755); err != nil {
		return fmt.Errorf("create policies dir: %w", err)
	}

	policyPath := filepath.Join(policyDir, "greenlight.toml")

	policy := `# Greenlight: auto-approve tools so the BeforeTool hook handles permissions
[[rule]]
toolName = "*"
decision = "allow"
priority = 999
`
	if err := os.WriteFile(policyPath, []byte(policy), 0644); err != nil {
		return fmt.Errorf("write %s: %w", policyPath, err)
	}

	log.Printf("Installed gemini policy in %s", policyPath)
	return nil
}

// upsertGreenlightHook takes the existing hook array for an event and either
// updates the greenlight entry or appends it. Non-greenlight hooks are preserved.
func upsertGreenlightHook(existing interface{}, hookEntry []interface{}, hookCmd string) []interface{} {
	arr, ok := existing.([]interface{})
	if !ok || len(arr) == 0 {
		return hookEntry
	}

	// Look for an existing greenlight hook entry and replace it
	found := false
	result := make([]interface{}, 0, len(arr))
	for _, entry := range arr {
		m, ok := entry.(map[string]interface{})
		if !ok {
			result = append(result, entry)
			continue
		}
		if isGreenlightHookEntry(m) {
			if !found {
				result = append(result, hookEntry[0])
				found = true
			}
			// Skip duplicate greenlight entries
		} else {
			result = append(result, entry)
		}
	}
	if !found {
		result = append(result, hookEntry[0])
	}
	return result
}

// removeGreenlightHooks removes all greenlight hook entries, returning only non-greenlight ones.
func removeGreenlightHooks(existing interface{}) []interface{} {
	arr, ok := existing.([]interface{})
	if !ok {
		return nil
	}
	var result []interface{}
	for _, entry := range arr {
		m, ok := entry.(map[string]interface{})
		if !ok {
			result = append(result, entry)
			continue
		}
		if !isGreenlightHookEntry(m) {
			result = append(result, entry)
		}
	}
	return result
}

// isGreenlightHookEntry checks if a hook matcher entry contains a greenlight hook command.
func isGreenlightHookEntry(entry map[string]interface{}) bool {
	hooks, ok := entry["hooks"].([]interface{})
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		bin := cmd
		if i := strings.IndexByte(cmd, ' '); i >= 0 {
			bin = cmd[:i]
		}
		if strings.Contains(filepath.Base(bin), "greenlight") {
			return true
		}
	}
	return false
}
