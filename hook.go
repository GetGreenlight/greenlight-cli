//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hookInput is the JSON structure received from Claude Code on stdin.
type hookInput struct {
	HookEventName    string          `json:"hook_event_name"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	NotificationType string          `json:"notification_type"`
	Message          string          `json:"message"`
	Title            string          `json:"title"`
}

func runHook(args []string) {
	baseURL, err := serverBaseURL()
	if err != nil {
		denyAndExit("Greenlight server not configured: " + err.Error())
	}

	// Resolve device ID: env > config file
	deviceID := os.Getenv("GREENLIGHT_DEVICE_ID")
	if deviceID == "" {
		deviceID = readConfigValue("device_id")
	}
	if deviceID == "" {
		denyAndExit("Greenlight device ID not configured. See https://getgreenlight.github.io/support.html")
	}

	project := os.Getenv("GREENLIGHT_PROJECT")
	if project == "" {
		denyAndExit("Greenlight project not configured. Run: greenlight connect --project PROJECT_NAME")
	}

	relayID := os.Getenv("GREENLIGHT_SESSION_ID")

	// Read hook input from stdin
	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		denyAndExit("Failed to read hook input: " + err.Error())
	}

	var input hookInput
	if err := json.Unmarshal(inputData, &input); err != nil {
		denyAndExit("Failed to parse hook input: " + err.Error())
	}

	// Default event type
	if input.HookEventName == "" {
		input.HookEventName = "PermissionRequest"
	}

	log.Printf("hook: event=%s session=%s relay=%s", input.HookEventName, input.SessionID, relayID)

	// Fall back to Claude's session_id if no relay ID from env
	if relayID == "" {
		relayID = input.SessionID
	}

	switch input.HookEventName {
	case "SessionStart":
		handleSessionStart(baseURL, deviceID, project, relayID, input)
	case "PermissionRequest":
		handlePermissionRequest(baseURL, deviceID, project, relayID, input, inputData)
	case "Notification":
		handleNotification(baseURL, deviceID, project, relayID, input)
	default:
		// Unknown event — exit silently
		os.Exit(0)
	}
}

func handleSessionStart(baseURL, deviceID, project, relayID string, input hookInput) {
	// Export env vars to CLAUDE_ENV_FILE so subprocesses inherit them
	if envFile := os.Getenv("CLAUDE_ENV_FILE"); envFile != "" {
		var lines []string
		if relayID != "" {
			lines = append(lines, fmt.Sprintf("export GREENLIGHT_SESSION_ID=%q", relayID))
		}
		if project != "" {
			lines = append(lines, fmt.Sprintf("export GREENLIGHT_PROJECT=%q", project))
		}
		if len(lines) > 0 {
			f, err := os.OpenFile(envFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				for _, line := range lines {
					fmt.Fprintln(f, line)
				}
				f.Close()
			}
		}
	}

	if relayID == "" {
		os.Exit(0)
	}

	// Eagerly enroll session
	if err := enrollSessionWithMarker(baseURL, deviceID, relayID, project); err != nil {
		log.Printf("Session enrollment failed: %v", err)
		os.Exit(0)
	}

	// Send session_start activity event
	payload := map[string]interface{}{
		"device_id":  deviceID,
		"event":      "session_start",
		"tool_name":  "SessionStart",
		"tool_input": map[string]interface{}{},
		"project":    project,
		"relay_id":   relayID,
		"agent":      "claude-code",
	}
	go func() {
		postJSON(baseURL+"/activity", payload, 10*time.Second)
	}()

	// Persist conversation → relay mapping so resumed sessions reuse the same relay ID
	if input.SessionID != "" && relayID != "" {
		saveRelayID(input.SessionID, relayID)
	}

	// Start transcript streamer if transcript path is available
	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = relayID
	}
	transcriptPath := input.TranscriptPath
	if transcriptPath != "" {
		maybeStartStreamer(baseURL, deviceID, project, relayID, sessionID, transcriptPath)
	}

	os.Exit(0)
}

func handlePermissionRequest(baseURL, deviceID, project, relayID string, input hookInput, rawInput []byte) {
	// Start transcript streamer if not already running
	if relayID != "" && input.TranscriptPath != "" {
		enrollSessionWithMarker(baseURL, deviceID, relayID, project)
		maybeStartStreamer(baseURL, deviceID, project, relayID, input.SessionID, input.TranscriptPath)
	}

	// Build payload: merge original input with our metadata
	var payload map[string]interface{}
	if err := json.Unmarshal(rawInput, &payload); err != nil {
		denyAndExit("Failed to parse hook input: " + err.Error())
	}
	payload["device_id"] = deviceID
	payload["project"] = project
	payload["relay_id"] = relayID
	payload["agent"] = "claude-code"

	// Send to server (long-poll)
	resp, err := postJSON(baseURL+"/request", payload, 595*time.Second)
	if err != nil {
		denyInterruptAndExit("Failed to reach Greenlight server (timeout or connection error)")
	}
	defer resp.Body.Close()

	// Handle 401 — enroll and retry
	if resp.StatusCode == 401 && relayID != "" {
		clearEnrollmentMarker(relayID)
		if err := enrollSessionWithMarker(baseURL, deviceID, relayID, project); err != nil {
			denyAndExit("Greenlight session enrollment was rejected")
		}
		// Retry
		resp.Body.Close()
		resp, err = postJSON(baseURL+"/request", payload, 595*time.Second)
		if err != nil {
			denyInterruptAndExit("Failed to reach Greenlight server (timeout or connection error)")
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		denyAndExit(fmt.Sprintf("Greenlight server error (HTTP %d): %s", resp.StatusCode, string(body)))
	}

	// Parse response
	var serverResp struct {
		Behavior     string                 `json:"behavior"`
		Message      string                 `json:"message"`
		UpdatedInput map[string]interface{} `json:"updated_input"`
		Interrupt    bool                   `json:"interrupt"`
		Error        string                 `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		denyAndExit("Failed to parse server response: " + err.Error())
	}

	if serverResp.Error != "" {
		denyAndExit(serverResp.Error)
	}

	if serverResp.Behavior == "allow" {
		if len(serverResp.UpdatedInput) > 0 {
			allowWithUpdatedInput(serverResp.UpdatedInput)
		} else {
			allowAndExit()
		}
	} else {
		msg := serverResp.Message
		if msg == "" {
			msg = "Permission denied"
		}
		if serverResp.Interrupt {
			denyInterruptAndExit(msg)
		} else {
			denyAndExit(msg)
		}
	}
}

func handleNotification(baseURL, deviceID, project, relayID string, input hookInput) {
	toolInput := map[string]string{
		"notification_type": input.NotificationType,
		"message":           input.Message,
		"title":             input.Title,
	}

	payload := map[string]interface{}{
		"device_id":  deviceID,
		"tool_name":  input.NotificationType,
		"tool_input": toolInput,
		"relay_id":   relayID,
		"agent":      "claude-code",
	}
	if project != "" {
		payload["project"] = project
	}

	// Fire-and-forget
	go func() {
		postJSON(baseURL+"/request", payload, 10*time.Second)
	}()

	os.Exit(0)
}

// enrollSessionWithMarker enrolls the session if not already enrolled (marker file check).
func enrollSessionWithMarker(baseURL, deviceID, relayID, project string) error {
	marker := filepath.Join(os.TempDir(), "greenlight-enrolled-"+relayID)
	if _, err := os.Stat(marker); err == nil {
		return nil // already enrolled
	}
	if err := enrollSession(baseURL, deviceID, relayID, project); err != nil {
		return err
	}
	os.WriteFile(marker, nil, 0644)
	return nil
}

func clearEnrollmentMarker(relayID string) {
	marker := filepath.Join(os.TempDir(), "greenlight-enrolled-"+relayID)
	os.Remove(marker)
}

// maybeStartStreamer starts the transcript streamer subprocess if not already running.
func maybeStartStreamer(baseURL, deviceID, project, relayID, sessionID, transcriptPath string) {
	if transcriptPath == "" || sessionID == "" {
		return
	}

	// Note: transcript file may not exist yet at SessionStart time.
	// The streamer subprocess will wait for it to appear.

	pidFile := filepath.Join(os.TempDir(), "greenlight-stream-"+sessionID+".pid")

	// Check existing streamer
	if data, err := os.ReadFile(pidFile); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 2 {
			pid, _ := strconv.Atoi(parts[0])
			existingRelay := parts[1]
			if pid > 0 && existingRelay == relayID {
				// Check if process is still alive
				if proc, err := os.FindProcess(pid); err == nil {
					if proc.Signal(nil) == nil {
						return // streamer already running with correct relay ID
					}
				}
			}
			// Kill stale streamer
			if pid > 0 {
				if proc, err := os.FindProcess(pid); err == nil {
					proc.Signal(os.Kill)
				}
			}
		}
	}

	// Spawn greenlight stream as a detached subprocess
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("Failed to resolve executable: %v", err)
		return
	}
	// Resolve symlinks so we invoke the real binary (not greenlight-hook symlink)
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	// Use bridge file if available (connect tails it over WebSocket),
	// otherwise fall back to direct HTTP POST
	var cmdArgs []string
	if bridgePath := os.Getenv("GREENLIGHT_BRIDGE"); bridgePath != "" {
		cmdArgs = []string{"stream",
			"--transcript", transcriptPath,
			"--session-id", sessionID,
			"--relay-id", relayID,
			"--bridge", bridgePath,
		}
	} else {
		cmdArgs = []string{"stream",
			"--transcript", transcriptPath,
			"--session-id", sessionID,
			"--device-id", deviceID,
			"--project", project,
			"--relay-id", relayID,
			"--server", baseURL,
		}
	}
	cmd := exec.Command(exePath, cmdArgs...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start streamer: %v", err)
		return
	}

	// Write PID file
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %s", cmd.Process.Pid, relayID)), 0644)

	// Don't wait for the child — it's detached
	cmd.Process.Release()
}

// Hook output helpers

func denyAndExit(message string) {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": "deny",
				"message":  message,
			},
		},
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

func denyInterruptAndExit(message string) {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior":  "deny",
				"message":   message,
				"interrupt": true,
			},
		},
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

func allowAndExit() {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": "allow",
			},
		},
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

func allowWithUpdatedInput(updatedInput map[string]interface{}) {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior":     "allow",
				"updatedInput": updatedInput,
			},
		},
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

// detachedSysProcAttr returns SysProcAttr for a detached subprocess.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}
