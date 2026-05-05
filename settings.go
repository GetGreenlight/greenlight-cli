//go:build darwin || linux

package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// installGreenlightInstructions creates an agent-specific instruction file
// that teaches the agent how to interpret [GREENLIGHT] permission denial messages.
func installGreenlightInstructions(agent, relayID, cwd string) error {
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

	content := "<!-- Greenlight -->\n" + greenlightSystemPrompt + "\n"
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
