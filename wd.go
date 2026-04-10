//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// apiHarness mirrors the server's Harness struct for JSON decoding.
type apiHarness struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// apiAIBrainModel mirrors the server's AIBrainModel struct for JSON decoding.
type apiAIBrainModel struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Description string `json:"description,omitempty"`
}

// apiWorkingDirectory mirrors the server's WorkingDirectory struct for JSON decoding.
type apiWorkingDirectory struct {
	ID            int    `json:"id"`
	Name          string `json:"name,omitempty"`
	DirectoryPath string `json:"directory_path,omitempty"`
	HarnessID     *int   `json:"harness_id,omitempty"`
	ModelID       *int   `json:"model_id,omitempty"`
	Active        bool   `json:"active"`
	CreatedAt     string `json:"created_at"`
}

func runWD(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, `Usage: greenlight wd <command> [flags]

Commands:
  create    Create a new working directory entry

Run 'greenlight wd <command> --help' for details.
`)
		os.Exit(0)
	}
	switch args[0] {
	case "create":
		runWDCreate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "greenlight wd: unknown command %q\nRun 'greenlight wd --help' for usage.\n", args[0])
		os.Exit(1)
	}
}

func runWDCreate(args []string) {
	fs := flag.NewFlagSet("wd create", flag.ExitOnError)
	deviceIDFlag := fs.String("device-id", "", "Device ID (overrides GREENLIGHT_DEVICE_ID env and config file)")
	nameFlag := fs.String("name", "", "Human name for this working directory (e.g. \"Alice\")")
	harnessFlag := fs.String("harness", "", "Agent harness name (e.g. claude-code, windsurf)")
	modelFlag := fs.String("model", "", "AI model name (e.g. claude-sonnet-4-6)")
	dirFlag := fs.String("dir", "", "Directory path (defaults to current directory)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: greenlight wd create [flags]

Creates a new working directory entry in the server database.
When flags are omitted, an interactive prompt guides you through each field.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	// Resolve device ID
	deviceID := *deviceIDFlag
	if deviceID == "" {
		deviceID = os.Getenv("GREENLIGHT_DEVICE_ID")
	}
	if deviceID == "" {
		deviceID = readConfigValue("device_id")
	}
	if deviceID == "" {
		fmt.Fprintf(os.Stderr, "greenlight: device ID required (set via --device-id, GREENLIGHT_DEVICE_ID, or 'greenlight register')\n")
		os.Exit(1)
	}

	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	// Fetch reference data from server
	harnesses, err := fetchHarnesses(baseURL, deviceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to fetch harnesses: %v\n", err)
		os.Exit(1)
	}
	models, err := fetchAIBrainModels(baseURL, deviceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to fetch models: %v\n", err)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	// --- Name ---
	name := *nameFlag
	if name == "" {
		name = promptLine(reader, "Name (optional, e.g. \"Alice\"): ")
	}

	// --- Harness ---
	var selectedHarness *apiHarness
	if *harnessFlag != "" {
		h := findHarnessByName(harnesses, *harnessFlag)
		if h == nil {
			fmt.Fprintf(os.Stderr, "greenlight: unknown harness %q\n", *harnessFlag)
			os.Exit(1)
		}
		selectedHarness = h
	} else {
		fmt.Println("\nAvailable harnesses:")
		for i, h := range harnesses {
			if h.Description != "" {
				fmt.Printf("  %2d. %-22s %s\n", i+1, h.Name, h.Description)
			} else {
				fmt.Printf("  %2d. %s\n", i+1, h.Name)
			}
		}
		defaultHarness := "claude-code"
		choice := promptWithDefault(reader, "\nHarness", defaultHarness)
		h := findHarnessByName(harnesses, choice)
		if h == nil {
			fmt.Fprintf(os.Stderr, "greenlight: unknown harness %q\n", choice)
			os.Exit(1)
		}
		selectedHarness = h
	}

	// --- Model ---
	var selectedModel *apiAIBrainModel
	if *modelFlag != "" {
		m := findModelByName(models, *modelFlag)
		if m == nil {
			fmt.Fprintf(os.Stderr, "greenlight: unknown model %q\n", *modelFlag)
			os.Exit(1)
		}
		selectedModel = m
	} else {
		provider := harnessProvider(selectedHarness.Name)
		var filtered []apiAIBrainModel
		for _, m := range models {
			if provider == "" || m.Provider == provider {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) == 0 {
			filtered = models
		}
		fmt.Println("\nAvailable models:")
		for i, m := range filtered {
			if m.Description != "" {
				fmt.Printf("  %2d. %-38s %s\n", i+1, m.Name, m.Description)
			} else {
				fmt.Printf("  %2d. %s\n", i+1, m.Name)
			}
		}
		defaultModel := defaultModelForHarness(selectedHarness.Name)
		choice := promptWithDefault(reader, "\nModel", defaultModel)
		m := findModelByName(models, choice)
		if m == nil {
			fmt.Fprintf(os.Stderr, "greenlight: unknown model %q\n", choice)
			os.Exit(1)
		}
		selectedModel = m
	}

	// --- Directory path ---
	dirPath := *dirFlag
	if dirPath == "" {
		cwd, _ := os.Getwd()
		dirPath = promptWithDefault(reader, "\nDirectory path", cwd)
	}

	// --- Create ---
	fmt.Println()
	wd, err := createWorkingDirectory(baseURL, deviceID, name, dirPath, selectedHarness.ID, selectedModel.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to create working directory: %v\n", err)
		os.Exit(1)
	}

	label := fmt.Sprintf("#%d", wd.ID)
	if wd.Name != "" {
		label += " \"" + wd.Name + "\""
	}
	fmt.Printf("Created working directory %s\n", label)
	if wd.DirectoryPath != "" {
		fmt.Printf("  path:    %s\n", wd.DirectoryPath)
	}
	if selectedHarness != nil {
		fmt.Printf("  harness: %s\n", selectedHarness.Name)
	}
	if selectedModel != nil {
		fmt.Printf("  model:   %s\n", selectedModel.Name)
	}
}

// --- HTTP helpers ---

func bearerClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func fetchHarnesses(baseURL, deviceID string) ([]apiHarness, error) {
	req, _ := http.NewRequest("GET", baseURL+"/organization/harnesses", nil)
	req.Header.Set("Authorization", "Bearer "+deviceID)
	resp, err := bearerClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var results []apiHarness
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}

func fetchAIBrainModels(baseURL, deviceID string) ([]apiAIBrainModel, error) {
	req, _ := http.NewRequest("GET", baseURL+"/organization/ai_brain_models", nil)
	req.Header.Set("Authorization", "Bearer "+deviceID)
	resp, err := bearerClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var results []apiAIBrainModel
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}

func createWorkingDirectory(baseURL, deviceID, name, dirPath string, harnessID, modelID int) (*apiWorkingDirectory, error) {
	payload := map[string]interface{}{
		"harness_id":     harnessID,
		"model_id":       modelID,
		"directory_path": dirPath,
	}
	if name != "" {
		payload["name"] = name
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("POST", baseURL+"/organization/working_directories", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+deviceID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := bearerClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var wd apiWorkingDirectory
	if err := json.NewDecoder(resp.Body).Decode(&wd); err != nil {
		return nil, err
	}
	return &wd, nil
}

// --- Prompt helpers ---

func promptLine(r *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func promptWithDefault(r *bufio.Reader, label, def string) string {
	fmt.Printf("%s [%s]: ", label, def)
	line, _ := r.ReadString('\n')
	s := strings.TrimSpace(line)
	if s == "" {
		return def
	}
	return s
}

// --- Lookup helpers ---

func findHarnessByName(harnesses []apiHarness, name string) *apiHarness {
	for i := range harnesses {
		if harnesses[i].Name == name {
			return &harnesses[i]
		}
	}
	return nil
}

func findModelByName(models []apiAIBrainModel, name string) *apiAIBrainModel {
	for i := range models {
		if models[i].Name == name {
			return &models[i]
		}
	}
	return nil
}

// harnessProvider maps a harness name to its expected model provider.
func harnessProvider(harness string) string {
	switch harness {
	case "claude-code", "windsurf":
		return "anthropic"
	case "codex", "openai-assistants", "cursor":
		return "openai"
	case "gemini":
		return "google"
	default:
		return ""
	}
}

// defaultModelForHarness returns a sensible default model name for a given harness.
func defaultModelForHarness(harness string) string {
	switch harness {
	case "claude-code", "windsurf", "cursor":
		return "claude-sonnet-4-6"
	case "codex", "openai-assistants":
		return "o4-mini"
	case "gemini":
		return "gemini-2.5-pro"
	default:
		return ""
	}
}
