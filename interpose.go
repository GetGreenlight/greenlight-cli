//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// interposeRequest is the JSON structure sent by the interpose library.
type interposeRequest struct {
	Type    string   `json:"type"`              // "open", "spawn", "connect", "read"
	Path    string   `json:"path,omitempty"`    // file path or binary path
	OldPath string   `json:"old_path,omitempty"` // source path for rename edits
	Args    []string `json:"args,omitempty"`    // argv for spawn
	Flags   string   `json:"flags,omitempty"`   // "w", "rw", "rename" for open
	Host    string   `json:"host,omitempty"`    // hostname for connect
	IP      string   `json:"ip,omitempty"`      // IP for connect
	Port    int      `json:"port,omitempty"`    // port for connect
	PID     int      `json:"pid,omitempty"`
	Project *bool    `json:"project,omitempty"` // true if file is within project dir
}

// interposeResponse is sent back to the interpose library.
type interposeResponse struct {
	Allow     bool   `json:"allow"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Message   string `json:"message,omitempty"`
}


// interposeRelay is a per-socket reference to the session's Relay.
// Created by startInterposeSock, set via SetRelay after the Relay is created.
type interposeRelay struct {
	mu sync.Mutex
	r  *Relay
}

func (ir *interposeRelay) SetRelay(r *Relay) {
	ir.mu.Lock()
	ir.r = r
	ir.mu.Unlock()
}

func (ir *interposeRelay) ClearRelay(r *Relay) {
	ir.mu.Lock()
	if ir.r == r {
		ir.r = nil
	}
	ir.mu.Unlock()
}

func (ir *interposeRelay) GetRelay() *Relay {
	ir.mu.Lock()
	r := ir.r
	ir.mu.Unlock()
	return r
}

// formatToolDetail returns a human-readable summary of the tool input for display.
func formatToolDetail(toolName string, toolInput map[string]interface{}) string {
	switch toolName {
	case "Bash":
		if cmd, ok := toolInput["command"].(string); ok {
			return cmd
		}
	case "Read", "Write", "Edit":
		if fp, ok := toolInput["file_path"].(string); ok {
			return fp
		}
	case "WebFetch":
		if u, ok := toolInput["url"].(string); ok {
			return u
		}
	}
	return fmt.Sprintf("%v", toolInput)
}

// startInterposeSock creates a Unix socket listener for interpose permission
// requests and returns the socket path and a per-session relay reference.
// The caller must call SetRelay on the returned interposeRelay once the
// Relay is created.
func startInterposeSock(relayID, agent string) (string, func(), *interposeRelay, error) {
	// Unix socket paths are limited to ~104 bytes on macOS, keep it short
	sockPath := "/tmp/gl-" + relayID[:8] + ".sock"

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		// If the socket file exists but nobody is listening, it's stale — remove and retry.
		if conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond); dialErr != nil {
			os.Remove(sockPath)
			listener, err = net.Listen("unix", sockPath)
		} else {
			conn.Close()
			// Socket is live — another session owns it
		}
		if err != nil {
			return "", nil, nil, fmt.Errorf("interpose socket %s: %w", sockPath, err)
		}
	}

	ir := &interposeRelay{}
	go handleInterposeSock(listener, agent, ir)

	cleanup := func() {
		listener.Close()
		// os.Remove not needed — listener.Close() removes the socket file
	}
	return sockPath, cleanup, ir, nil
}

func handleInterposeSock(listener net.Listener, agent string, ir *interposeRelay) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return // listener closed
		}
		go handleInterposeConn(conn, agent, ir)
	}
}

// isSafeCommand checks if a Bash command can be auto-allowed without
// prompting the user. A command is safe if its base binary is read-only
// AND it contains no output redirects (which could write to files).
func isSafeCommand(cmd string) bool {
	// Check for output redirects: any unquoted > means the command can
	// write to a file, even if the binary itself is read-only.
	inSingle := false
	inDouble := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if c == '\\' && inDouble && i+1 < len(cmd) {
			i++ // skip escaped char
			continue
		}
		if c == '>' && !inSingle && !inDouble {
			return false
		}
	}

	// Extract the base command name (skip env assignments like VAR=val)
	fields := strings.Fields(cmd)
	cmdName := ""
	for _, f := range fields {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue // env var assignment
		}
		cmdName = f
		break
	}
	if cmdName == "" {
		return false
	}
	// Use basename
	if idx := strings.LastIndex(cmdName, "/"); idx >= 0 {
		cmdName = cmdName[idx+1:]
	}

	// Read-only commands that cannot modify files without redirects
	switch cmdName {
	case "pwd", "echo", "printf", "wc", "ls", "cat", "head", "tail",
		"grep", "awk", "find", "file", "stat", "realpath", "readlink",
		"dirname", "basename", "which", "command", "type",
		"ps", "uname", "whoami", "hostname", "id", "arch", "nproc",
		"uptime", "df", "free", "env", "printenv",
		"defaults", "system_profiler", "security", "ioreg",
		"dpkg", "lsb_release", "lscpu", "lsblk",
		"date", "cal", "true", "false", "test", "[",
		"sort", "uniq", "tr", "cut", "less", "more":
		return true
	case "greenlight", "greenlight-dev":
		return isSafeGreenlightSubcommand(fields, cmdName)
	}
	return false
}

// isSafeGreenlightSubcommand returns true for read-only / internal-callback
// greenlight subcommands that should auto-allow without a permission request.
// Mutating subcommands (run, connect, register, secrets init/get/set/rm,
// agent <name>) fall through and require approval. Mirrors the C-side check
// in interpose.c::is_safe_shell_command.
func isSafeGreenlightSubcommand(fields []string, cmdName string) bool {
	// Find the subcommand: first non-env-assignment field after cmdName.
	sub := ""
	rest := []string{}
	seenCmd := false
	for _, f := range fields {
		if !seenCmd {
			if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
				continue
			}
			seenCmd = true
			continue
		}
		if sub == "" {
			sub = f
			continue
		}
		rest = append(rest, f)
	}
	_ = cmdName
	switch sub {
	case "", "version", "--version", "-v", "hook":
		return true
	case "agent":
		// Bare `agent` is a query; with an arg it sets config.
		return len(rest) == 0
	case "secrets":
		return len(rest) > 0 && rest[0] == "list"
	}
	return false
}

func handleInterposeConn(conn net.Conn, agent string, ir *interposeRelay) {
	defer conn.Close()

	// Try to receive with ancillary data (SCM_RIGHTS for seccomp fd)
	line, seccompFd := recvWithAncillary(conn)
	if line == nil {
		respond(conn, interposeResponse{Allow: false})
		return
	}

	var req interposeRequest
	if err := json.Unmarshal(line, &req); err != nil {
		log.Printf("Interpose: bad request: %v", err)
		respond(conn, interposeResponse{Allow: false})
		return
	}

	// Handle seccomp fd handoff (Linux only)
	if req.Type == "seccomp_fd" && seccompFd >= 0 {
		log.Printf("Interpose: received seccomp notification fd %d", seccompFd)
		go runSeccompSupervisor(seccompFd, agent, ir)
		return
	}

	log.Printf("Interpose: %s %s", req.Type, interposeRequestSummary(req))

	// Translate to server permission request format
	toolName, toolInput := translateInterposeRequest(req)

	// Auto-allow safe commands (read-only with no output redirects)
	if toolName == "Bash" {
		if cmd, ok := toolInput["command"].(string); ok && isSafeCommand(cmd) {
			log.Printf("Interpose: auto-allow safe command: %s", cmd)
			respond(conn, interposeResponse{Allow: true})
			return
		}
	}

	payload := map[string]interface{}{
		"agent":           agent,
		"hook_event_name": "PermissionRequest",
		"tool_name":       toolName,
		"tool_input":      toolInput,
	}
	if req.Project != nil {
		payload["project_file"] = *req.Project
	}

	// Get the relay for WebSocket and terminal prompt access
	relay := ir.GetRelay()

	if relay == nil || relay.wsConn == nil {
		log.Printf("Interpose: no relay/websocket available, denying")
		respond(conn, interposeResponse{Allow: false})
		return
	}

	resp := racePermission(relay, toolName, toolInput, payload)
	log.Printf("Interpose: %s %s → %v", toolName, interposeRequestSummary(req),
		map[bool]string{true: "allow", false: "deny"}[resp.Allow])
	respond(conn, resp)
}

// wsPermission sends a permission request over the WebSocket and waits for
// the server's response. Returns the response or an error if ctx is cancelled.
func wsPermission(ctx context.Context, ws WSConn, requestID string, payload map[string]interface{}) (interposeResponse, error) {
	respCh := ws.RegisterPending(requestID)
	defer ws.RemovePending(requestID)

	// Copy payload and add request_id (avoid mutating shared map)
	data := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		data[k] = v
	}
	data["request_id"] = requestID

	msg := map[string]interface{}{
		"type": "permission_request",
		"data": data,
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		return interposeResponse{Allow: false}, err
	}

	ws.SendText(encoded)

	select {
	case respData := <-respCh:
		var resp struct {
			Behavior  string `json:"behavior"`
			Message   string `json:"message"`
			Interrupt bool   `json:"interrupt"`
		}
		if err := json.Unmarshal(respData, &resp); err != nil {
			return interposeResponse{Allow: false}, err
		}
		return interposeResponse{
			Allow:     resp.Behavior == "allow",
			Interrupt: resp.Interrupt,
			Message:   resp.Message,
		}, nil
	case <-ctx.Done():
		return interposeResponse{Allow: false}, ctx.Err()
	}
}

// racePermission sends the permission request to the server via WebSocket.
// If the server doesn't respond quickly (e.g. waiting for phone approval),
// a terminal prompt is shown. Whichever responds first wins.
func racePermission(relay *Relay, toolName string, toolInput map[string]interface{}, payload map[string]interface{}) interposeResponse {
	type result struct {
		resp    interposeResponse
		source  string
		outcome string // terminal choice: allow, always_allow, deny, deny_stop
	}
	ch := make(chan result, 2)
	requestID := generateUUID()

	ws := relay.wsConn
	wsCtx, wsCancel := context.WithCancel(context.Background())
	defer wsCancel()

	go func() {
		log.Printf("Interpose: sending WS permission request %s for %s", requestID, toolName)
		r, err := wsPermission(wsCtx, ws, requestID, payload)
		if err != nil {
			if wsCtx.Err() != nil {
				return // cancelled by terminal
			}
			log.Printf("Interpose: WS permission error for %s (req %s): %v", toolName, requestID, err)
			ch <- result{resp: interposeResponse{Allow: false}, source: "server"}
			return
		}
		ch <- result{resp: r, source: "server"}
	}()

	// Wait for a fast server response (auto-approve).
	select {
	case r := <-ch:
		log.Printf("Interpose: permission %s via %s (fast)",
			map[bool]string{true: "allowed", false: "denied"}[r.resp.Allow], r.source)
		return r.resp
	case <-relay.shutdownCh:
		log.Printf("Interpose: relay shutting down, denying %s", toolName)
		wsCancel()
		return interposeResponse{Allow: false}
	case <-time.After(200 * time.Millisecond):
	}

	// Server is waiting for phone approval — show terminal prompt
	promptCtx, promptCancel := context.WithCancel(context.Background())
	defer promptCancel()

	log.Printf("Interpose: showing terminal prompt for %s", toolName)
	detail := formatToolDetail(toolName, toolInput)
	// Terminal prompt choices: 0=allow, 1=always_allow, 2=deny, 3=deny_stop
	var terminalOutcomes = []string{"allow", "always_allow", "deny", "deny_stop"}

	go func() {
		choice, err := relay.ShowPromptDaemon(promptCtx, toolName, detail)
		if err != nil {
			if promptCtx.Err() != nil {
				return // ctx cancelled, server won
			}
			log.Printf("Interpose: ShowPromptDaemon error for %s (req %s): %v",
				toolName, requestID, err)
			return
		}
		outcome := terminalOutcomes[choice]
		ch <- result{
			resp: interposeResponse{
				Allow:     outcome == "allow" || outcome == "always_allow",
				Interrupt: outcome == "deny_stop",
			},
			source:  "terminal",
			outcome: outcome,
		}
	}()

	// Wait for either terminal or server response, relay shutdown, or safety timeout
	var r result
	select {
	case r = <-ch:
	case <-relay.shutdownCh:
		log.Printf("Interpose: relay shutting down, denying %s", toolName)
		wsCancel()
		promptCancel()
		return interposeResponse{Allow: false}
	case <-time.After(600 * time.Second):
		log.Printf("Interpose: permission request timed out for %s", toolName)
		wsCancel()
		return interposeResponse{Allow: false}
	}
	log.Printf("Interpose: permission %s via %s",
		map[bool]string{true: "allowed", false: "denied"}[r.resp.Allow], r.source)

	// If the terminal won, cancel the pending server request
	if r.source == "terminal" && ws != nil {
		cancel := map[string]interface{}{
			"type": "cancel_request",
			"data": map[string]interface{}{
				"request_id": requestID,
				"outcome":    r.outcome,
			},
		}
		if data, err := json.Marshal(cancel); err == nil {
			ws.SendText(data)
		}
		wsCancel() // unblock wsPermission goroutine
	}

	return r.resp
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
			// For shell -c, the command is typically in args[2].
			// Only look for -c when the binary is actually a shell,
			// since other tools (e.g. git -c key=val) use -c for
			// different purposes.
			shellCmd := ""
			cursorCmd := ""
			base := filepath.Base(req.Path)
			isShell := base == "sh" || base == "bash" || base == "zsh" ||
				base == "dash" || base == "ksh" || base == "fish"
			for i, a := range req.Args {
				if isShell && a == "-c" && i+1 < len(req.Args) {
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
	quote := byte(0)
	if len(inner) > 0 && (inner[0] == '\'' || inner[0] == '"') {
		quote = inner[0]
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
	// Unescape \" sequences regardless of outer quote.
	inner = strings.ReplaceAll(inner, "\\\"", "\"")
	// When the outer wrapper was single-quoted, claude escapes literal `'`
	// inside CMD using one of the standard SQ-escape idioms. After we strip
	// the outer SQ, those sequences must collapse back to a literal `'` or
	// the resulting string is a parse error for any downstream shell parser.
	if quote == '\'' {
		inner = strings.ReplaceAll(inner, `'"'"'`, "'")
		inner = strings.ReplaceAll(inner, `'\''`, "'")
	}
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

// recvWithAncillary reads a line from the connection, optionally extracting
// an SCM_RIGHTS fd (used on Linux for seccomp notification fd handoff).
// Returns the line bytes and the fd (-1 if none).
func recvWithAncillary(conn net.Conn) ([]byte, int) {
	if runtime.GOOS == "linux" {
		// Try recvmsg to get ancillary data (SCM_RIGHTS)
		unixConn, ok := conn.(*net.UnixConn)
		if ok {
			rawConn, err := unixConn.SyscallConn()
			if err == nil {
				// Sized to match the interpose library's GL_MAX_REQ_BYTES
				// (128 KiB) plus slack for the path/pid framing. Anything
				// larger is denied by the C side before reaching us.
				var buf [160 * 1024]byte
				oob := make([]byte, syscall.CmsgSpace(4))
				var n, oobn int
				var recvErr error

				rawConn.Read(func(fd uintptr) bool {
					n, oobn, _, _, recvErr = syscall.Recvmsg(int(fd), buf[:], oob, 0)
					return recvErr != syscall.EAGAIN
				})

				if recvErr == nil && n > 0 {
					seccompFd := -1
					if oobn > 0 {
						msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
						if err == nil {
							for _, msg := range msgs {
								if msg.Header.Level == syscall.SOL_SOCKET && msg.Header.Type == syscall.SCM_RIGHTS {
									fds, err := syscall.ParseUnixRights(&msg)
									if err == nil && len(fds) > 0 {
										seccompFd = fds[0]
									}
								}
							}
						}
					}
					return buf[:n], seccompFd
				}
			}
		}
	}

	// Fallback: normal buffered read
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, -1
	}
	return line, -1
}
