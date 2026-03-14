//go:build darwin || linux

package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// installGeminiPolicy creates a policy file that auto-approves all tools,
// letting interpose handle permissions instead of the gemini CLI's built-in prompt.
func installGeminiPolicy() error {
	policyDir := filepath.Join(".gemini", "policies")
	if err := os.MkdirAll(policyDir, 0755); err != nil {
		return err
	}

	policyPath := filepath.Join(policyDir, "greenlight.toml")

	policy := `# Greenlight: auto-approve tools so interpose handles permissions
[[rule]]
toolName = "*"
decision = "allow"
priority = 999
`
	if err := os.WriteFile(policyPath, []byte(policy), 0644); err != nil {
		return err
	}

	log.Printf("Installed gemini policy in %s", policyPath)
	return nil
}

// installGreenlightInstructions creates an agent-specific instruction file
// that teaches the agent how to interpret [GREENLIGHT] permission denial messages.
func installGreenlightInstructions(agent string) error {
	var instrPath string
	switch agent {
	case "gemini":
		instrPath = "GEMINI.md"
	case "copilot":
		if err := os.MkdirAll(".github", 0755); err != nil {
			return err
		}
		instrPath = filepath.Join(".github", "copilot-instructions.md")
	case "cursor":
		if err := os.MkdirAll(filepath.Join(".cursor", "rules"), 0755); err != nil {
			return err
		}
		instrPath = filepath.Join(".cursor", "rules", "greenlight.mdc")
	case "codex":
		instrPath = "AGENTS.md"
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
