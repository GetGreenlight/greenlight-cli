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

// hookInput is the JSON structure received from the agent CLI on stdin.
type hookInput struct {
	HookEventName    string          `json:"hook_event_name"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	NotificationType string          `json:"notification_type"`
	Message          string          `json:"message"`
	Title            string          `json:"title"`
	// Gemini-specific fields
	Details string `json:"details"`
}

// copilotHookInput is the JSON structure received from copilot-cli on stdin.
type copilotHookInput struct {
	Timestamp     int64           `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	ToolName      string          `json:"toolName"`
	ToolArgs      json.RawMessage `json:"toolArgs"` // JSON string or object
	Source        string          `json:"source"`
	InitialPrompt string         `json:"initialPrompt"`
}

func runHook(args []string) {
	// Set output format early so error helpers use the right format
	hookOutputAgent = resolveAgent("")

	baseURL, err := serverBaseURL()
	if err != nil {
		denyAndExit("Greenlight server not configured: " + err.Error())
	}

	// Resolve device ID: env > config file
	// If not configured, allow immediately — the hook is installed but
	// greenlight is not actively managing this session.
	deviceID := os.Getenv("GREENLIGHT_DEVICE_ID")
	if deviceID == "" {
		deviceID = readConfigValue("device_id")
	}
	if deviceID == "" {
		allowAndExit()
	}

	project := os.Getenv("GREENLIGHT_PROJECT")
	if project == "" {
		allowAndExit()
	}

	rawAgent := resolveAgent("")
	agent := agentServerName(rawAgent)

	relayID := os.Getenv("GREENLIGHT_SESSION_ID")

	// Read hook input from stdin
	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		denyAndExit("Failed to read hook input: " + err.Error())
	}

	var input hookInput

	// Copilot passes the event type as a CLI argument (sessionStart, preToolUse)
	// and uses a different stdin JSON format.
	if rawAgent == "copilot" && len(args) > 0 {
		copilotEvent := args[0]
		input = parseCopilotInput(inputData, copilotEvent)
	} else {
		if err := json.Unmarshal(inputData, &input); err != nil {
			denyAndExit("Failed to parse hook input: " + err.Error())
		}
	}

	// Default event type
	if input.HookEventName == "" {
		input.HookEventName = "PermissionRequest"
	}

	log.Printf("hook: event=%s session=%s relay=%s agent=%s", input.HookEventName, input.SessionID, relayID, agent)

	// Fall back to agent's session_id if no relay ID from env
	if relayID == "" {
		relayID = input.SessionID
	}

	// Re-serialize inputData for copilot so handlePermissionRequest
	// sends the normalized payload to the server
	if rawAgent == "copilot" {
		if normalized, err := json.Marshal(map[string]interface{}{
			"hook_event_name": input.HookEventName,
			"tool_name":       input.ToolName,
			"tool_input":      json.RawMessage(input.ToolInput),
		}); err == nil {
			inputData = normalized
		}
	}

	switch input.HookEventName {
	case "SessionStart":
		handleSessionStart(baseURL, deviceID, project, relayID, agent, rawAgent, input)
	case "PermissionRequest", "BeforeTool":
		handlePermissionRequest(baseURL, deviceID, project, relayID, agent, rawAgent, input, inputData)
	case "Notification":
		handleNotification(baseURL, deviceID, project, relayID, agent, input)
	default:
		// Unknown event — exit silently
		os.Exit(0)
	}
}

func handleSessionStart(baseURL, deviceID, project, relayID, agent, rawAgent string, input hookInput) {
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
		"agent":      agent,
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
	if transcriptPath == "" {
		transcriptPath = deriveTranscriptPath(agent, sessionID)
	}
	if transcriptPath != "" {
		maybeStartStreamer(baseURL, deviceID, project, relayID, sessionID, transcriptPath, rawAgent)
	}

	os.Exit(0)
}

func handlePermissionRequest(baseURL, deviceID, project, relayID, agent, rawAgent string, input hookInput, rawInput []byte) {
	// Start transcript streamer if not already running
	transcriptPath := input.TranscriptPath
	if transcriptPath == "" {
		transcriptPath = deriveTranscriptPath(agent, input.SessionID)
	}
	if relayID != "" && transcriptPath != "" {
		enrollSessionWithMarker(baseURL, deviceID, relayID, project)
		maybeStartStreamer(baseURL, deviceID, project, relayID, input.SessionID, transcriptPath, rawAgent)
	}

	// Build payload: merge original input with our metadata
	var payload map[string]interface{}
	if err := json.Unmarshal(rawInput, &payload); err != nil {
		denyAndExit("Failed to parse hook input: " + err.Error())
	}
	payload["device_id"] = deviceID
	payload["project"] = project
	payload["relay_id"] = relayID
	payload["agent"] = agent

	// Normalize Gemini tool names to Claude equivalents
	if agent == "gemini" {
		if toolName, ok := payload["tool_name"].(string); ok {
			if mapped, found := geminiToolNameMap[toolName]; found {
				payload["tool_name"] = mapped
			} else {
				// Wrap unrecognized tools as Generic/GenericSafe
				payload["tool_input"] = map[string]interface{}{
					"toolName": toolName,
					"args":     payload["tool_input"],
				}
				if geminiSafeToolSet[toolName] {
					payload["tool_name"] = "GenericSafe"
				} else {
					payload["tool_name"] = "Generic"
				}
			}
		}
	}

	// Normalize Copilot tool names and args to Claude equivalents
	if agent == "copilot" {
		if toolName, ok := payload["tool_name"].(string); ok {
			if mapped, found := copilotToolNameMap[toolName]; found {
				// Known tool — rename and normalize args
				payload["tool_name"] = mapped
				if ti, ok := payload["tool_input"].(map[string]interface{}); ok {
					payload["tool_input"] = normalizeCopilotArgs(mapped, ti)
				}
			} else if copilotSafeToolSet[toolName] {
				payload["tool_input"] = map[string]interface{}{
					"toolName": toolName,
					"args":     payload["tool_input"],
				}
				payload["tool_name"] = "GenericSafe"
			} else {
				// Unknown tool — wrap as Generic
				payload["tool_input"] = map[string]interface{}{
					"toolName": toolName,
					"args":     payload["tool_input"],
				}
				payload["tool_name"] = "Generic"
			}
		}
	}

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

func handleNotification(baseURL, deviceID, project, relayID, agent string, input hookInput) {
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
		"agent":      agent,
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
func maybeStartStreamer(baseURL, deviceID, project, relayID, sessionID, transcriptPath, agent string) {
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
			"--agent", agent,
		}
	} else {
		cmdArgs = []string{"stream",
			"--transcript", transcriptPath,
			"--session-id", sessionID,
			"--device-id", deviceID,
			"--project", project,
			"--relay-id", relayID,
			"--server", baseURL,
			"--agent", agent,
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

// geminiToolNameMap translates Gemini tool names to their Claude equivalents
// so the server and client can use a single set of tool name display logic.
// Unmapped tools are sent as "Generic" with the original name in tool_input.
// Tools in geminiSafeToolSet use "GenericSafe" (server converts args to globs).
var geminiToolNameMap = map[string]string{
	"read_file":         "Read",
	"write_file":        "Write",
	"replace":           "Edit",
	"run_shell_command": "Bash",
	"grep_search":       "Grep",
	"list_directory":    "ListDirectory",
	"web_fetch":         "WebFetch",
	"google_web_search": "WebSearch",
	"get_internal_docs": "Read",
}

var geminiSafeToolSet = map[string]bool{
	"cli_help": true,
}

// copilotToolNameMap translates Copilot CLI tool names to their Claude equivalents.
// Copilot sends lowercase names in hook stdin; these map to PascalCase Claude names.
var copilotToolNameMap = map[string]string{
	"bash":       "Bash",
	"shell":      "Bash",
	"edit":       "Edit",
	"view":       "Read",
	"read":       "Read",
	"create":     "Write",
	"write":      "Write",
	"grep":       "Grep",
	"rg":         "Grep",
	"glob":       "Glob",
	"web_fetch":  "WebFetch",
	"web_search": "WebSearch",
}

// copilotSafeToolSet lists copilot tools that are safe (no side effects).
var copilotSafeToolSet = map[string]bool{
	"ReportIntent": true,
	"AskUser":      true,
}

// normalizeCopilotArgs renames copilot tool_input keys to Claude equivalents
// so the phone app can display arguments correctly.
func normalizeCopilotArgs(toolName string, toolInput map[string]interface{}) map[string]interface{} {
	switch toolName {
	case "Read", "Grep", "Glob":
		renameKey(toolInput, "path", "file_path")
	case "Edit":
		renameKey(toolInput, "path", "file_path")
		renameKey(toolInput, "old_str", "old_string")
		renameKey(toolInput, "new_str", "new_string")
	case "Write":
		renameKey(toolInput, "path", "file_path")
		renameKey(toolInput, "file_text", "content")
	}
	return toolInput
}

func renameKey(m map[string]interface{}, old, new string) {
	if v, ok := m[old]; ok {
		m[new] = v
		delete(m, old)
	}
}

// parseCopilotInput converts copilot-cli's hook stdin JSON into our internal hookInput.
func parseCopilotInput(data []byte, event string) hookInput {
	var ci copilotHookInput
	json.Unmarshal(data, &ci)

	var input hookInput
	switch event {
	case "sessionStart":
		input.HookEventName = "SessionStart"
	case "preToolUse":
		input.HookEventName = "PermissionRequest"
		input.ToolName = ci.ToolName
		// toolArgs may be a JSON string (quoted) or a JSON object
		if len(ci.ToolArgs) > 0 {
			if ci.ToolArgs[0] == '"' {
				// JSON string — unwrap the quotes to get the inner JSON
				var s string
				if json.Unmarshal(ci.ToolArgs, &s) == nil {
					input.ToolInput = json.RawMessage(s)
				}
			} else {
				// JSON object — use directly
				input.ToolInput = ci.ToolArgs
			}
		}
	default:
		input.HookEventName = event
	}

	return input
}

// Hook output helpers
//
// hookOutputAgent controls the output format. Set before calling any
// output helper. Defaults to "claude" (Claude Code format). For "gemini",
// uses top-level decision/reason fields.
var hookOutputAgent = "claude"

func denyAndExit(message string) {
	var output interface{}
	switch hookOutputAgent {
	case "gemini":
		output = map[string]interface{}{
			"decision": "deny",
			"reason":   message,
		}
	case "copilot":
		output = map[string]interface{}{
			"permissionDecision":       "deny",
			"permissionDecisionReason": message,
		}
	default:
		output = map[string]interface{}{
			"hookSpecificOutput": map[string]interface{}{
				"hookEventName": "PermissionRequest",
				"decision": map[string]interface{}{
					"behavior": "deny",
					"message":  message,
				},
			},
		}
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

func denyInterruptAndExit(message string) {
	var output interface{}
	switch hookOutputAgent {
	case "gemini":
		output = map[string]interface{}{
			"decision":   "deny",
			"reason":     message,
			"continue":   false,
			"stopReason": message,
		}
	case "copilot":
		// Copilot has no interrupt concept — deny is the strongest signal
		output = map[string]interface{}{
			"permissionDecision":       "deny",
			"permissionDecisionReason": message,
		}
	default:
		output = map[string]interface{}{
			"hookSpecificOutput": map[string]interface{}{
				"hookEventName": "PermissionRequest",
				"decision": map[string]interface{}{
					"behavior":  "deny",
					"message":   message,
					"interrupt": true,
				},
			},
		}
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

func allowAndExit() {
	var output interface{}
	switch hookOutputAgent {
	case "gemini":
		output = map[string]interface{}{
			"decision": "allow",
		}
	case "copilot":
		// Copilot treats no output as allow; output explicitly for consistency
		output = map[string]interface{}{
			"permissionDecision": "allow",
		}
	default:
		output = map[string]interface{}{
			"hookSpecificOutput": map[string]interface{}{
				"hookEventName": "PermissionRequest",
				"decision": map[string]interface{}{
					"behavior": "allow",
				},
			},
		}
	}
	json.NewEncoder(os.Stdout).Encode(output)
	os.Exit(0)
}

func allowWithUpdatedInput(updatedInput map[string]interface{}) {
	var output interface{}
	switch hookOutputAgent {
	case "gemini":
		output = map[string]interface{}{
			"decision": "allow",
			"hookSpecificOutput": map[string]interface{}{
				"tool_input": updatedInput,
			},
		}
	case "copilot":
		// Copilot doesn't support updated input — just allow
		output = map[string]interface{}{
			"permissionDecision": "allow",
		}
	default:
		output = map[string]interface{}{
			"hookSpecificOutput": map[string]interface{}{
				"hookEventName": "PermissionRequest",
				"decision": map[string]interface{}{
					"behavior":     "allow",
					"updatedInput": updatedInput,
				},
			},
		}
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
