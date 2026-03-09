//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// interposeRequest is the JSON structure sent by the interpose library.
type interposeRequest struct {
	Type    string   `json:"type"`              // "open", "spawn", "connect"
	Path    string   `json:"path,omitempty"`    // file path or binary path
	OldPath string   `json:"old_path,omitempty"` // source path for rename edits
	Args    []string `json:"args,omitempty"`    // argv for spawn
	Flags   string   `json:"flags,omitempty"`   // "w", "rw", "rename" for open
	Host    string   `json:"host,omitempty"`    // hostname for connect
	IP      string   `json:"ip,omitempty"`      // IP for connect
	Port    int      `json:"port,omitempty"`    // port for connect
	PID     int      `json:"pid,omitempty"`
}

// interposeResponse is sent back to the interpose library.
type interposeResponse struct {
	Allow     bool   `json:"allow"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Message   string `json:"message,omitempty"`
}

// requestThrottle spaces out permission requests to the server,
// preventing 429 rate limit cascades when Claude reads many files at once.
var requestThrottle = struct {
	sync.Mutex
	last time.Time
}{}

func throttleRequest() {
	requestThrottle.Lock()
	if since := time.Since(requestThrottle.last); since < 100*time.Millisecond {
		time.Sleep(100*time.Millisecond - since)
	}
	requestThrottle.last = time.Now()
	requestThrottle.Unlock()
}

// startInterposeSock creates a Unix socket listener for interpose permission
// requests and returns the socket path. The listener is handled in a goroutine.
// Returns empty string if setup fails.
func startInterposeSock(relayID, baseURL, deviceID, project, agent string) (string, func()) {
	// Unix socket paths are limited to ~104 bytes on macOS, keep it short
	sockPath := "/tmp/gl-" + relayID[:8] + ".sock"

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Printf("Interpose socket: failed to create %s: %v", sockPath, err)
		return "", nil
	}

	go handleInterposeSock(listener, baseURL, deviceID, project, relayID, agent)

	cleanup := func() {
		listener.Close()
		// os.Remove not needed — listener.Close() removes the socket file
	}
	return sockPath, cleanup
}

func handleInterposeSock(listener net.Listener, baseURL, deviceID, project, relayID, agent string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return // listener closed
		}
		go handleInterposeConn(conn, baseURL, deviceID, project, relayID, agent)
	}
}

func handleInterposeConn(conn net.Conn, baseURL, deviceID, project, relayID, agent string) {
	defer conn.Close()

	// Read one line (the request)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		respond(conn, interposeResponse{Allow: false})
		return
	}

	var req interposeRequest
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("Interpose: bad request: %v", err)
		respond(conn, interposeResponse{Allow: false})
		return
	}

	log.Printf("Interpose: %s %s", req.Type, interposeRequestSummary(req))

	// Translate to server permission request format
	toolName, toolInput := translateInterposeRequest(req)

	payload := map[string]interface{}{
		"device_id":       deviceID,
		"project":         project,
		"relay_id":        relayID,
		"agent":           agent,
		"hook_event_name": "PermissionRequest",
		"tool_name":       toolName,
		"tool_input":      toolInput,
	}

	// Send to server (long-poll, same timeout as hooks) with retry on 429
	var resp *http.Response
	for attempt := 0; attempt < 4; attempt++ {
		// Space out requests to prevent burst overload (429s)
		throttleRequest()
		var err error
		resp, err = postJSON(baseURL+"/request", payload, 595*time.Second)
		if err != nil {
			log.Printf("Interpose: server error: %v", err)
			respond(conn, interposeResponse{Allow: false})
			return
		}

		// Handle 401 — re-enroll and retry
		if resp.StatusCode == 401 && relayID != "" {
			clearEnrollmentMarker(relayID)
			if err := enrollSessionWithMarker(baseURL, deviceID, relayID, project); err != nil {
				resp.Body.Close()
				respond(conn, interposeResponse{Allow: false})
				return
			}
			resp.Body.Close()
			continue
		}

		// Retry on 429 with exponential backoff
		if resp.StatusCode == 429 {
			resp.Body.Close()
			backoff := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
			log.Printf("Interpose: 429 rate limited, retrying in %v", backoff)
			time.Sleep(backoff)
			continue
		}

		break
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Interpose: server HTTP %d: %s", resp.StatusCode, string(body))
		respond(conn, interposeResponse{Allow: false})
		return
	}

	var serverResp struct {
		Behavior  string `json:"behavior"`
		Message   string `json:"message"`
		Interrupt bool   `json:"interrupt"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		log.Printf("Interpose: bad server response: %v", err)
		respond(conn, interposeResponse{Allow: false})
		return
	}

	if serverResp.Error != "" {
		log.Printf("Interpose: server error: %s", serverResp.Error)
		respond(conn, interposeResponse{Allow: false})
		return
	}

	r := interposeResponse{
		Allow:     serverResp.Behavior == "allow",
		Interrupt: serverResp.Interrupt,
		Message:   serverResp.Message,
	}
	respond(conn, r)
}

func respond(conn net.Conn, r interposeResponse) {
	data, _ := json.Marshal(r)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.Write(data)
}

// translateInterposeRequest converts an interpose event to the server's
// tool_name + tool_input format, matching the hook permission request schema.
func translateInterposeRequest(req interposeRequest) (string, map[string]interface{}) {
	switch req.Type {
	case "read":
		return "Read", map[string]interface{}{
			"file_path": req.Path,
		}
	case "open":
		// Rename with old_path means the target already exists — it's an edit.
		// Compute old/new strings for display (matches Claude's Edit format).
		if req.Flags == "rename" && req.OldPath != "" {
			oldStr, newStr := computeRenameEdit(req.Path, req.OldPath)
			if oldStr != "" || newStr != "" {
				return "Edit", map[string]interface{}{
					"file_path":  req.Path,
					"old_string": oldStr,
					"new_string": newStr,
				}
			}
			return "Edit", map[string]interface{}{
				"file_path": req.Path,
			}
		}
		// If the file already exists, it's an edit; otherwise a write/create.
		toolName := "Write"
		if _, err := os.Stat(req.Path); err == nil {
			toolName = "Edit"
		}
		return toolName, map[string]interface{}{
			"file_path": req.Path,
		}
	case "spawn":
		// Codex apply_patch: the codex binary is spawned with patch content as args.
		// Translate as an Edit tool with the filename from the patch.
		if strings.Contains(req.Path, "codex") && len(req.Args) > 1 &&
			strings.Contains(req.Args[len(req.Args)-1], "*** Begin Patch") {
			patch := req.Args[len(req.Args)-1]
			// Extract filename and old/new from patch content
			filePath := "unknown"
			if idx := strings.Index(patch, "*** Update File: "); idx >= 0 {
				line := patch[idx+17:]
				if nl := strings.IndexByte(line, '\n'); nl >= 0 {
					filePath = line[:nl]
				}
			} else if idx := strings.Index(patch, "*** Add File: "); idx >= 0 {
				line := patch[idx+14:]
				if nl := strings.IndexByte(line, '\n'); nl >= 0 {
					filePath = line[:nl]
				}
			}
			oldStr, newStr := extractPatchStrings(patch)
			return "Edit", map[string]interface{}{
				"file_path":  filePath,
				"old_string": oldStr,
				"new_string": newStr,
			}
		}

		// Join args as the command string
		cmd := ""
		if len(req.Args) > 0 {
			// For shell -c, the command is typically in args[2]
			shellCmd := ""
			cursorCmd := ""
			for i, a := range req.Args {
				if a == "-c" && i+1 < len(req.Args) {
					shellCmd = req.Args[i+1]
				}
				// Cursor wrapper: actual command is the arg after "--"
				if a == "--" && i+1 < len(req.Args) {
					cursorCmd = req.Args[i+1]
				}
			}
			// Cursor puts the actual command after "--"; use it if the
			// shell script is the cursor wrapper (contains builtin eval "$1")
			if cursorCmd != "" && strings.Contains(shellCmd, `builtin eval "$1"`) {
				cmd = cursorCmd
			} else if shellCmd != "" {
				cmd = shellCmd
			}
			if cmd == "" {
				// Not a shell -c, join all args
				for i, a := range req.Args {
					if i > 0 {
						cmd += " "
					}
					cmd += a
				}
			}
		}
		// Unwrap Claude's eval wrapper:
		//   source .../shell-snapshots/... && setopt ... && eval 'CMD' < /dev/null && pwd ...
		cmd = unwrapEvalCommand(cmd)
		// Unwrap Gemini's shopt wrapper:
		//   shopt -u ...; { ACTUAL_CMD }; __code=$?; ...
		cmd = unwrapGeminiCommand(cmd)
		// Unwrap Codex's shell snapshot wrapper:
		//   if . '.codex/shell_snapshots/UUID.sh' ...; fi\n\nexec '/bin/zsh' -c 'ACTUAL_CMD'
		cmd = unwrapCodexCommand(cmd)
		return "Bash", map[string]interface{}{
			"command": cmd,
		}
	case "connect":
		url := "https://" + req.Host
		if req.Port != 0 && req.Port != 443 {
			url = req.IP
		}
		return "WebFetch", map[string]interface{}{
			"url": url,
		}
	default:
		return "Generic", map[string]interface{}{
			"type": req.Type,
			"path": req.Path,
		}
	}
}

// unwrapEvalCommand extracts the actual command from Claude's Bash tool wrapper.
// Claude wraps commands as: source .../shell-snapshots/... && setopt ... && eval 'CMD' \< /dev/null && pwd ...
func unwrapEvalCommand(cmd string) string {
	idx := strings.Index(cmd, "&& eval ")
	if idx < 0 {
		return cmd
	}
	inner := cmd[idx+8:] // skip "&& eval "
	// Find the command between quotes, handling escaped quotes
	if len(inner) > 0 && (inner[0] == '\'' || inner[0] == '"') {
		quote := inner[0]
		inner = inner[1:]
		// Find closing delimiter: \< /dev/null or matching unescaped quote at end
		// Claude uses: eval 'cmd' \< /dev/null && pwd ...
		// or: eval "cmd" \< /dev/null && pwd ...
		endMarker := string(quote) + " \\< /dev/null"
		if end := strings.Index(inner, endMarker); end >= 0 {
			inner = inner[:end]
		} else if end := strings.LastIndexByte(inner, quote); end >= 0 {
			inner = inner[:end]
		}
	}
	// Unescape \" sequences
	inner = strings.ReplaceAll(inner, "\\\"", "\"")
	if inner == "" {
		return cmd
	}
	return inner
}

// unwrapGeminiCommand extracts the actual command from Gemini's shell wrapper.
// Gemini wraps commands as: shopt -u ...; { ACTUAL_CMD }; __code=$?; ...
func unwrapGeminiCommand(cmd string) string {
	if !strings.HasPrefix(cmd, "shopt ") {
		return cmd
	}
	// Find "{ " and extract up to "};"
	braceStart := strings.Index(cmd, "{ ")
	if braceStart < 0 {
		return cmd
	}
	inner := cmd[braceStart+2:]
	// Find the closing "};" — but the inner command may contain "}" too,
	// so find "}; __code" which is Gemini's specific suffix
	if end := strings.Index(inner, "}; __code"); end >= 0 {
		inner = strings.TrimSpace(inner[:end])
	} else if end := strings.LastIndex(inner, "};"); end >= 0 {
		inner = strings.TrimSpace(inner[:end])
	}
	if inner == "" {
		return cmd
	}
	return inner
}

// unwrapCodexCommand extracts the actual command from Codex's shell wrapper.
// Codex wraps commands as:
//
//	if . '/path/.codex/shell_snapshots/UUID.sh' >/dev/null 2>&1; then :; fi\n\nexec '/bin/zsh' -c 'ACTUAL_CMD'
//
// We extract ACTUAL_CMD from the exec line.
func unwrapCodexCommand(cmd string) string {
	// Look for the exec pattern after a newline
	idx := strings.Index(cmd, "\nexec '")
	if idx < 0 {
		return cmd
	}
	execLine := cmd[idx+1:] // skip \n
	// Pattern: exec '/bin/zsh' -c 'ACTUAL_CMD'
	// Find -c ' and extract the command
	cIdx := strings.Index(execLine, " -c '")
	if cIdx < 0 {
		return cmd
	}
	inner := execLine[cIdx+5:] // skip " -c '"
	// Find the closing quote — but the command may contain escaped quotes
	// Codex uses '\'' to escape single quotes in single-quoted strings
	// For display purposes, just trim the trailing quote
	if len(inner) > 0 && inner[len(inner)-1] == '\'' {
		inner = inner[:len(inner)-1]
	}
	if inner == "" {
		return cmd
	}
	return inner
}

// extractPatchStrings parses a Codex apply_patch format and returns the
// removed lines and added lines as old_string/new_string.
// Format: lines starting with "-" are removed, "+" are added.
func extractPatchStrings(patch string) (string, string) {
	var oldLines, newLines []string
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "-") {
			oldLines = append(oldLines, line[1:])
		} else if strings.HasPrefix(line, "+") {
			newLines = append(newLines, line[1:])
		}
	}
	return strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
}

// computeRenameEdit reads the existing file (target) and the temp file (source),
// extracts the changed lines, and returns old/new strings matching Claude's Edit
// tool format. Returns ("","") on any error or if the files are identical.
func computeRenameEdit(targetPath, sourcePath string) (string, string) {
	oldBytes, err := os.ReadFile(targetPath)
	if err != nil {
		return "", ""
	}
	newBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", ""
	}
	oldContent := string(oldBytes)
	newContent := string(newBytes)
	if oldContent == newContent {
		return "", ""
	}

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Find first differing line
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}

	// Find last differing line (from the end)
	endOld := len(oldLines) - 1
	endNew := len(newLines) - 1
	for endOld > start && endNew > start && oldLines[endOld] == newLines[endNew] {
		endOld--
		endNew--
	}

	oldStr := strings.Join(oldLines[start:endOld+1], "\n")
	newStr := strings.Join(newLines[start:endNew+1], "\n")

	// Truncate very large strings
	if len(oldStr) > 2048 {
		oldStr = oldStr[:2048] + "\n..."
	}
	if len(newStr) > 2048 {
		newStr = newStr[:2048] + "\n..."
	}

	return oldStr, newStr
}

func interposeRequestSummary(req interposeRequest) string {
	switch req.Type {
	case "open":
		return req.Path + " (" + req.Flags + ")"
	case "spawn":
		// For Cursor wrapper, show the actual command (after --)
		for i, a := range req.Args {
			if a == "--" && i+1 < len(req.Args) {
				return req.Path + " " + req.Args[i+1]
			}
		}
		if len(req.Args) > 2 {
			return req.Path + " " + req.Args[len(req.Args)-1]
		}
		return req.Path
	case "connect":
		if req.Host != "" {
			return req.Host
		}
		return req.IP
	default:
		return req.Path
	}
}
