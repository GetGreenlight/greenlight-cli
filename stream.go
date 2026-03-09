//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runStream(args []string) {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	transcriptPath := fs.String("transcript", "", "Path to transcript file")
	sessionID := fs.String("session-id", "", "Session ID")
	deviceID := fs.String("device-id", "", "Device ID")
	project := fs.String("project", "", "Project name")
	relayID := fs.String("relay-id", "", "Relay ID")
	server := fs.String("server", "", "Server base URL")
	bridge := fs.String("bridge", "", "Bridge file path (write lines here instead of HTTP POST)")
	agentFlag := fs.String("agent", "", "Agent runtime (claude, gemini)")
	fs.Parse(args)

	if *transcriptPath == "" || *sessionID == "" {
		fmt.Fprintf(os.Stderr, "greenlight stream: missing required flags\n")
		os.Exit(1)
	}

	// Bridge mode: server and device-id are not required
	if *bridge == "" && (*deviceID == "" || *server == "") {
		fmt.Fprintf(os.Stderr, "greenlight stream: missing required flags (--server, --device-id or --bridge)\n")
		os.Exit(1)
	}

	// Write PID file for the hook to check
	pidFile := filepath.Join(os.TempDir(), "greenlight-stream-"+*sessionID+".pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %s", os.Getpid(), *relayID)), 0644)
	defer os.Remove(pidFile)

	agent := *agentFlag
	if agent == "" {
		agent = resolveAgent("")
	}

	if *bridge != "" {
		switch agent {
		case "gemini":
			streamGeminiBridge(*transcriptPath, *bridge)
		case "copilot":
			streamCopilotBridge(*transcriptPath, *bridge)
		case "cursor":
			streamCursorBridge(*transcriptPath, *bridge)
		case "codex":
			streamCodexBridge(*transcriptPath, *bridge)
		default:
			streamToBridge(*transcriptPath, *sessionID, *bridge)
		}
	} else {
		streamTranscript(*transcriptPath, *sessionID, *deviceID, *project, *relayID, *server)
	}
}

// streamToBridge tails a JSONL transcript file and appends each line to the bridge file.
// The bridge file is tailed by `connect` which sends lines over the relay WebSocket.
func streamToBridge(transcriptPath, sessionID, bridgePath string) {
	// Wait for transcript file to appear (may not exist at SessionStart)
	var f *os.File
	for i := 0; i < 300; i++ { // up to 30 seconds
		var err error
		f, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", transcriptPath)
		return
	}
	defer f.Close()

	// Start from beginning — transcript file is fresh for each session.
	// No seekToLastLines backfill needed, which avoids duplicates if
	// a second streamer is accidentally spawned.

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			// Complete line (delimiter found) — safe to write
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				// Write the raw JSONL line to the bridge file (one line per entry)
				if _, werr := fmt.Fprintln(bridge, fullLine); werr != nil {
					log.Printf("Bridge write error: %v", werr)
					return
				}
			}
		} else if line != "" {
			// Partial line (no newline yet) — buffer it
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// streamGeminiBridge polls a Gemini JSON transcript file for new messages,
// transforms them to Claude Code transcript format, and writes each as a
// JSONL line to the bridge file.
//
// On the first read, it parses the full file to extract sessionId and all
// messages. On subsequent reads, it only reads from the byte offset of the
// last message array entry, prepends '[', and parses just the new messages.
func streamGeminiBridge(transcriptPath, bridgePath string) {
	// Wait for transcript file to appear
	for i := 0; i < 300; i++ {
		if _, err := os.Stat(transcriptPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	var sessionID string
	var lastSize int64
	// Track raw content of each message so we detect when Gemini updates
	// an existing message (e.g. adding toolCalls after the text is written).
	var msgSnapshots []string

	for {
		info, err := os.Stat(transcriptPath)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if info.Size() == lastSize {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		lastSize = info.Size()

		// Re-read the full file each time. Gemini transcripts are single
		// JSON objects (not JSONL) that get rewritten in place, so
		// incremental parsing is fragile. Files are small enough (<100KB)
		// that a full re-read is fine.
		data, err := os.ReadFile(transcriptPath)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		var transcript struct {
			SessionID string            `json:"sessionId"`
			Messages  []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(data, &transcript); err != nil {
			// File may be mid-write; reset lastSize so we retry on next poll
			lastSize = 0
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if sessionID == "" {
			sessionID = transcript.SessionID
		}

		// Emit new messages and re-emit messages whose content changed
		// (Gemini writes text first, then adds toolCalls later).
		for i, raw := range transcript.Messages {
			snapshot := string(raw)
			if i < len(msgSnapshots) {
				if msgSnapshots[i] == snapshot {
					continue // unchanged
				}
				msgSnapshots[i] = snapshot
			} else {
				msgSnapshots = append(msgSnapshots, snapshot)
			}
			writeGeminiMessage(bridge, raw, sessionID)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// writeGeminiMessage transforms a gemini message and writes it to the bridge.
func writeGeminiMessage(bridge *os.File, raw json.RawMessage, sessionID string) {
	entries := transformGeminiMessage(raw, sessionID)
	for _, entry := range entries {
		compactBytes, err := json.Marshal(entry)
		if err != nil {
			log.Printf("gemini message marshal error: %v", err)
			continue
		}
		if _, werr := fmt.Fprintln(bridge, string(compactBytes)); werr != nil {
			log.Printf("Bridge write error: %v", werr)
		}
	}
}

// geminiMessage is the structure of a message in a Gemini transcript.
type geminiMessage struct {
	ID        string            `json:"id"`
	Timestamp string            `json:"timestamp"`
	Type      string            `json:"type"`
	Content   json.RawMessage   `json:"content"`
	Model     string            `json:"model"`
	Tokens    *geminiTokens     `json:"tokens,omitempty"`
	ToolCalls []geminiToolCall  `json:"toolCalls,omitempty"`
}

type geminiToolCall struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Args          map[string]interface{} `json:"args"`
	Result        json.RawMessage        `json:"result"`
	Status        string                 `json:"status"`
	Timestamp     string                 `json:"timestamp"`
	ResultDisplay json.RawMessage        `json:"resultDisplay"`
}

type geminiTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

// transformGeminiMessage converts a Gemini transcript message to Claude Code
// transcript format so the server/phone can render it uniformly.
// Returns a slice because a single Gemini message with toolCalls produces
// multiple entries: the assistant text + tool_use/tool_result pairs.
func transformGeminiMessage(raw json.RawMessage, sessionID string) []map[string]interface{} {
	var msg geminiMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		log.Printf("gemini message parse error: %v", err)
		return nil
	}

	switch msg.Type {
	case "user":
		// Gemini user content is [{text: "..."}], extract the text
		var contentParts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &contentParts); err != nil || len(contentParts) == 0 {
			return nil
		}
		text := contentParts[0].Text
		for _, p := range contentParts[1:] {
			text += "\n" + p.Text
		}
		return []map[string]interface{}{{
			"type":      "user",
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message": map[string]interface{}{
				"role":    "user",
				"content": text,
			},
		}}

	case "gemini":
		var entries []map[string]interface{}

		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			return nil
		}

		// Emit assistant text entry
		textEntry := map[string]interface{}{
			"type":      "assistant",
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": text},
				},
				"model": msg.Model,
			},
		}
		if msg.Tokens != nil {
			textEntry["message"].(map[string]interface{})["usage"] = map[string]interface{}{
				"input_tokens":  msg.Tokens.Input,
				"output_tokens": msg.Tokens.Output,
				"cache_read_input_tokens": msg.Tokens.Cached,
			}
		}
		entries = append(entries, textEntry)

		// Emit tool_use + tool_result pairs matching Claude's format
		for _, tc := range msg.ToolCalls {
			// tool_use: type "assistant" with content [{type:"tool_use",...}]
			entries = append(entries, map[string]interface{}{
				"type":      "assistant",
				"uuid":      tc.ID,
				"timestamp": tc.Timestamp,
				"sessionId": sessionID,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  normalizeGeminiToolName(tc.Name),
							"input": normalizeGeminiToolArgs(tc.Name, tc.Args),
						},
					},
				},
			})

			// tool_result: type "progress" with nested data.message.message
			resultContent := geminiToolResultContent(tc)
			entries = append(entries, map[string]interface{}{
				"type":      "progress",
				"uuid":      tc.ID + "_result",
				"timestamp": tc.Timestamp,
				"sessionId": sessionID,
				"data": map[string]interface{}{
					"message": map[string]interface{}{
						"type": "user",
						"message": map[string]interface{}{
							"role": "user",
							"content": []map[string]interface{}{
								{
									"type":        "tool_result",
									"tool_use_id": tc.ID,
									"content":     resultContent,
								},
							},
						},
					},
				},
			})
		}

		return entries

	default:
		// Other types — pass through with minimal wrapping
		var content interface{}
		json.Unmarshal(raw, &content)
		return []map[string]interface{}{{
			"type":      msg.Type,
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message":   content,
		}}
	}
}

// normalizeGeminiToolName maps Gemini tool names to Claude equivalents.
func normalizeGeminiToolName(name string) string {
	if mapped, ok := geminiToolNameMap[name]; ok {
		return mapped
	}
	return name
}

// normalizeGeminiToolArgs adjusts Gemini tool args to match Claude's format.
func normalizeGeminiToolArgs(name string, args map[string]interface{}) map[string]interface{} {
	switch name {
	case "run_shell_command":
		// Gemini uses "command" + "description"; Claude uses "command"
		return map[string]interface{}{
			"command": args["command"],
		}
	case "list_directory":
		// Map to Bash with "ls <dir>" command
		dir := ""
		if d, ok := args["dir_path"]; ok {
			dir = fmt.Sprintf("%v", d)
		}
		return map[string]interface{}{
			"command": "ls " + dir,
		}
	default:
		return args
	}
}

// geminiToolResultContent extracts a display-friendly result string from a tool call.
// For file operations with resultDisplay containing diffs, returns the diff.
// Otherwise returns the function response output or error.
func geminiToolResultContent(tc geminiToolCall) string {
	// Try resultDisplay for file diffs
	if len(tc.ResultDisplay) > 0 {
		// resultDisplay can be a string or an object with fileDiff
		var displayObj struct {
			FileDiff string `json:"fileDiff"`
		}
		if json.Unmarshal(tc.ResultDisplay, &displayObj) == nil && displayObj.FileDiff != "" {
			return displayObj.FileDiff
		}
		// Try as plain string
		var displayStr string
		if json.Unmarshal(tc.ResultDisplay, &displayStr) == nil && displayStr != "" {
			return displayStr
		}
	}

	// Fall back to function response output/error
	var results []struct {
		FunctionResponse struct {
			Response struct {
				Output string `json:"output"`
				Error  string `json:"error"`
			} `json:"response"`
		} `json:"functionResponse"`
	}
	if json.Unmarshal(tc.Result, &results) == nil && len(results) > 0 {
		resp := results[0].FunctionResponse.Response
		if resp.Error != "" {
			return resp.Error
		}
		return resp.Output
	}

	return ""
}

// streamCopilotBridge tails a copilot events.jsonl file, transforms each event
// to Claude transcript format, and writes the result to the bridge file.
// Copilot events have the structure:
//
//	{"type":"event.type","data":{...},"id":"uuid","timestamp":"ISO-8601","parentId":"uuid"}
func streamCopilotBridge(transcriptPath, bridgePath string) {
	// Wait for transcript file to appear
	var f *os.File
	for i := 0; i < 300; i++ {
		var err error
		f, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", transcriptPath)
		return
	}
	defer f.Close()

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				transformed := transformCopilotEvent(fullLine)
				if transformed != "" {
					if _, werr := fmt.Fprintln(bridge, transformed); werr != nil {
						log.Printf("Bridge write error: %v", werr)
						return
					}
				}
			}
		} else if line != "" {
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// transformCopilotEvent converts a copilot events.jsonl line to Claude transcript format.
func transformCopilotEvent(line string) string {
	var event struct {
		Type      string          `json:"type"`
		Data      json.RawMessage `json:"data"`
		ID        string          `json:"id"`
		Timestamp string          `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return ""
	}

	var entry map[string]interface{}

	switch event.Type {
	case "user.message":
		var data struct {
			Content string `json:"content"`
		}
		json.Unmarshal(event.Data, &data)
		entry = map[string]interface{}{
			"type":      "user",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role":    "user",
				"content": data.Content,
			},
		}

	case "assistant.message":
		var data struct {
			Content string `json:"content"`
			Model   string `json:"model"`
		}
		json.Unmarshal(event.Data, &data)
		entry = map[string]interface{}{
			"type":      "assistant",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": data.Content},
				},
				"model": data.Model,
			},
		}

	case "tool.execution_start":
		// Normalize to Claude tool_use format
		var data struct {
			ToolCallID string                 `json:"toolCallId"`
			ToolName   string                 `json:"toolName"`
			Arguments  map[string]interface{} `json:"arguments"`
		}
		json.Unmarshal(event.Data, &data)
		entry = map[string]interface{}{
			"type":      "assistant",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":  "tool_use",
						"name":  normalizeCopilotToolName(data.ToolName),
						"id":    data.ToolCallID,
						"input": normalizeCopilotToolArgs(data.ToolName, data.Arguments),
					},
				},
			},
		}

	case "tool.execution_complete":
		// Normalize to Claude tool_result format (type "progress" with nested structure)
		var data struct {
			ToolCallID string `json:"toolCallId"`
			Success    bool   `json:"success"`
			Result     struct {
				Content         string `json:"content"`
				DetailedContent string `json:"detailedContent"`
			} `json:"result"`
		}
		json.Unmarshal(event.Data, &data)
		// Prefer detailedContent (has diffs for edits) over content (short summary)
		resultContent := data.Result.Content
		if data.Result.DetailedContent != "" {
			resultContent = data.Result.DetailedContent
		}
		entry = map[string]interface{}{
			"type":      "progress",
			"uuid":      event.ID,
			"timestamp": event.Timestamp,
			"data": map[string]interface{}{
				"message": map[string]interface{}{
					"type": "user",
					"message": map[string]interface{}{
						"role": "user",
						"content": []map[string]interface{}{
							{
								"type":        "tool_result",
								"tool_use_id": data.ToolCallID,
								"content":     resultContent,
							},
						},
					},
				},
			},
		}

	default:
		return ""
	}

	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(out)
}

// normalizeCopilotToolName maps Copilot's lowercase tool names to Claude PascalCase.
func normalizeCopilotToolName(name string) string {
	switch name {
	case "bash":
		return "Bash"
	case "edit":
		return "Edit"
	case "view":
		return "Read"
	case "create":
		return "Write"
	default:
		return name
	}
}

// normalizeCopilotToolArgs translates Copilot arg keys to Claude equivalents.
func normalizeCopilotToolArgs(toolName string, args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		switch k {
		case "path":
			out["file_path"] = v
		case "file_text":
			out["content"] = v
		case "old_str":
			out["old_string"] = v
		case "new_str":
			out["new_string"] = v
		default:
			out[k] = v
		}
	}
	return out
}

// streamCursorBridge tails a Cursor agent-transcripts JSONL file, transforms each
// line to Claude transcript format, and writes to the bridge file.
// Cursor events have the structure:
//
//	{"role":"user|assistant","message":{"content":[{"type":"text","text":"..."}]}}
func streamCursorBridge(transcriptPath, bridgePath string) {
	// Wait for transcript file to appear
	var f *os.File
	for i := 0; i < 300; i++ {
		var err error
		f, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", transcriptPath)
		return
	}
	defer f.Close()

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	reader := bufio.NewReader(f)
	var partial string
	var partialAge int // number of consecutive EOF polls with unchanged partial

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			fullLine := trimNewline(partial + line)
			partial = ""
			partialAge = 0
			if fullLine != "" {
				transformed := transformCursorEvent(fullLine)
				if transformed != "" {
					if _, werr := fmt.Fprintln(bridge, transformed); werr != nil {
						log.Printf("Bridge write error: %v", werr)
						return
					}
				}
			}
		} else if line != "" {
			partial += line
			partialAge = 0
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			// Cursor may not write a trailing newline for the last line.
			// Flush the partial buffer after it's been stable for ~500ms.
			if partial != "" {
				partialAge++
				if partialAge >= 5 {
					transformed := transformCursorEvent(partial)
					if transformed != "" {
						if _, werr := fmt.Fprintln(bridge, transformed); werr != nil {
							log.Printf("Bridge write error: %v", werr)
							return
						}
					}
					partial = ""
					partialAge = 0
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// transformCursorEvent converts a Cursor agent-transcripts JSONL line to Claude
// transcript format.
func transformCursorEvent(line string) string {
	var event struct {
		Role    string `json:"role"`
		Message struct {
			Content json.RawMessage `json:"content"`
			Model   string          `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return ""
	}

	var entry map[string]interface{}

	switch event.Role {
	case "user":
		// Extract text from content array [{type:"text", text:"..."}]
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Message.Content, &parts); err != nil || len(parts) == 0 {
			return ""
		}
		text := parts[0].Text
		for _, p := range parts[1:] {
			text += "\n" + p.Text
		}
		// Strip Cursor's XML wrapper tags from user input
		text = stripCursorXMLTags(text)
		entry = map[string]interface{}{
			"type": "user",
			"message": map[string]interface{}{
				"role":    "user",
				"content": text,
			},
		}

	case "assistant":
		// Parse content to strip Cursor's bold status suffixes (e.g. "**Preparing response**")
		var parts []map[string]interface{}
		if err := json.Unmarshal(event.Message.Content, &parts); err != nil {
			return ""
		}
		for i, p := range parts {
			if t, ok := p["text"].(string); ok && p["type"] == "text" {
				parts[i]["text"] = stripCursorStatusSuffix(t)
			}
		}
		entry = map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": parts,
				"model":   event.Message.Model,
			},
		}

	default:
		return ""
	}

	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(out)
}

// stripCursorXMLTags removes Cursor's XML wrapper tags (e.g. <user_query>)
// from user text, returning just the inner content.
func stripCursorXMLTags(text string) string {
	// Strip <user_query>...</user_query> wrapper
	if start := strings.Index(text, "<user_query>"); start != -1 {
		inner := text[start+len("<user_query>"):]
		if end := strings.Index(inner, "</user_query>"); end != -1 {
			return strings.TrimSpace(inner[:end])
		}
	}
	return text
}

// stripCursorStatusSuffix removes Cursor's bold status text (e.g. "**Preparing response**")
// that gets appended to assistant messages but doesn't appear in the terminal.
func stripCursorStatusSuffix(text string) string {
	// Pattern: text ends with "\n\n**...**"
	idx := strings.LastIndex(text, "\n\n**")
	if idx == -1 {
		return text
	}
	suffix := text[idx+3:]
	if strings.HasSuffix(suffix, "**") && !strings.Contains(suffix[:len(suffix)-2], "**") {
		return text[:idx]
	}
	return text
}

// streamCodexBridge tails a Codex session JSONL file, transforms each event
// to Claude transcript format, and writes the result to the bridge file.
func streamCodexBridge(transcriptPath, bridgePath string) {
	// Wait for transcript file to appear
	var f *os.File
	for i := 0; i < 300; i++ {
		var err error
		f, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", transcriptPath)
		return
	}
	defer f.Close()

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				transformed := transformCodexEvent(fullLine)
				if transformed != "" {
					if _, werr := fmt.Fprintln(bridge, transformed); werr != nil {
						log.Printf("Bridge write error: %v", werr)
						return
					}
				}
			}
		} else if line != "" {
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// transformCodexEvent converts a Codex session JSONL line to Claude transcript format.
// Codex JSONL events use a top-level "type" field (response_item, event_msg, etc.)
// with a "payload" containing the actual data.
// extractCodexPatch parses a Codex apply_patch and returns removed/added lines.
func extractCodexPatch(patch string) (string, string) {
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

// normalizeCodexTool translates Codex tool names and arguments to Claude equivalents.
// exec_command → Bash (extract cmd), apply_patch → Edit, etc.
func normalizeCodexTool(name, arguments string) (string, interface{}) {
	switch name {
	case "exec_command":
		var args struct {
			Cmd string `json:"cmd"`
		}
		json.Unmarshal([]byte(arguments), &args)
		return "Bash", map[string]string{"command": args.Cmd}
	case "apply_patch":
		var args struct {
			Patch string `json:"patch"`
		}
		json.Unmarshal([]byte(arguments), &args)
		// Extract file path from patch content
		filePath := "unknown"
		if idx := strings.Index(args.Patch, "*** Update File: "); idx >= 0 {
			line := args.Patch[idx+17:]
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				filePath = line[:nl]
			}
		} else if idx := strings.Index(args.Patch, "*** Add File: "); idx >= 0 {
			line := args.Patch[idx+14:]
			if nl := strings.IndexByte(line, '\n'); nl >= 0 {
				filePath = line[:nl]
			}
		}
		oldStr, newStr := extractCodexPatch(args.Patch)
		return "Edit", map[string]interface{}{
			"file_path":  filePath,
			"old_string": oldStr,
			"new_string": newStr,
		}
	default:
		// Pass through unknown tools with raw arguments
		if arguments != "" {
			var parsed interface{}
			if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
				return name, parsed
			}
		}
		return name, map[string]string{}
	}
}

func transformCodexEvent(line string) string {
	var event struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return ""
	}

	var entry map[string]interface{}

	switch event.Type {
	case "response_item":
		// Contains messages, function calls, function call outputs, reasoning
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Content   json.RawMessage `json:"content"`
			Arguments string          `json:"arguments"` // function_call: JSON-encoded args
			Output    string          `json:"output"`
			Summary   json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(event.Payload, &item); err != nil {
			return ""
		}

		switch item.Type {
		case "message":
			if item.Role == "user" {
				// User message: content is an array of {type:"input_text", text:"..."}
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				json.Unmarshal(item.Content, &parts)
				text := ""
				for _, p := range parts {
					if p.Type == "input_text" && p.Text != "" {
						// Skip greenlight instruction content and system context
						if strings.Contains(p.Text, "<!-- Greenlight -->") ||
							strings.HasPrefix(p.Text, "<permissions") ||
							strings.HasPrefix(p.Text, "<environment_context>") ||
							strings.HasPrefix(p.Text, "<collaboration_mode>") {
							continue
						}
						if text != "" {
							text += "\n"
						}
						text += p.Text
					}
				}
				if text == "" {
					return ""
				}
				entry = map[string]interface{}{
					"type":      "user",
					"timestamp": event.Timestamp,
					"message": map[string]interface{}{
						"role":    "user",
						"content": text,
					},
				}
			} else if item.Role == "assistant" {
				// Assistant message: content is an array of {type:"output_text", text:"..."}
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				json.Unmarshal(item.Content, &parts)
				text := ""
				for _, p := range parts {
					if p.Type == "output_text" && p.Text != "" {
						if text != "" {
							text += "\n"
						}
						text += p.Text
					}
				}
				if text == "" {
					return ""
				}
				entry = map[string]interface{}{
					"type":      "assistant",
					"timestamp": event.Timestamp,
					"message": map[string]interface{}{
						"role": "assistant",
						"content": []map[string]interface{}{
							{"type": "text", "text": text},
						},
					},
				}
			} else {
				// Developer messages are system instructions — skip
				return ""
			}

		case "function_call":
			// Tool call: name + arguments (arguments is a JSON string)
			// Normalize Codex tool names to Claude equivalents
			toolName, toolInput := normalizeCodexTool(item.Name, item.Arguments)
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"name":  toolName,
							"id":    item.CallID,
							"input": toolInput,
						},
					},
				},
			}

		case "function_call_output":
			// Tool result (matches Claude's "progress" format)
			entry = map[string]interface{}{
				"type":      "progress",
				"timestamp": event.Timestamp,
				"data": map[string]interface{}{
					"message": map[string]interface{}{
						"type": "user",
						"message": map[string]interface{}{
							"role": "user",
							"content": []map[string]interface{}{
								{
									"type":        "tool_result",
									"tool_use_id": item.CallID,
									"content":     item.Output,
								},
							},
						},
					},
				},
			}

		case "web_search_call":
			// Web search: has action.query
			var search struct {
				Action struct {
					Query string `json:"query"`
				} `json:"action"`
			}
			json.Unmarshal(event.Payload, &search)
			query := search.Action.Query
			if query == "" {
				return ""
			}
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{
							"type":  "tool_use",
							"name":  "WebSearch",
							"input": map[string]string{"query": query},
						},
					},
				},
			}

		case "reasoning":
			// Model reasoning summary
			var summaryParts []struct {
				Text string `json:"text"`
			}
			json.Unmarshal(item.Summary, &summaryParts)
			text := ""
			for _, p := range summaryParts {
				if text != "" {
					text += "\n"
				}
				text += p.Text
			}
			if text == "" {
				return ""
			}
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "thinking", "thinking": text},
					},
				},
			}

		default:
			return ""
		}

	case "event_msg":
		// Agent events: user_message, agent_reasoning, token_count, task_started
		var msg struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Text    string `json:"text"`
		}
		json.Unmarshal(event.Payload, &msg)

		switch msg.Type {
		case "user_message":
			entry = map[string]interface{}{
				"type":      "user",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role":    "user",
					"content": msg.Message,
				},
			}
		case "agent_reasoning":
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "thinking", "thinking": msg.Text},
					},
				},
			}
		case "agent_message":
			if msg.Message == "" {
				return ""
			}
			entry = map[string]interface{}{
				"type":      "assistant",
				"timestamp": event.Timestamp,
				"message": map[string]interface{}{
					"role": "assistant",
					"content": []map[string]interface{}{
						{"type": "text", "text": msg.Message},
					},
				},
			}
		default:
			return ""
		}

	default:
		return ""
	}

	if entry == nil {
		return ""
	}
	out, err := json.Marshal(entry)
	if err != nil {
		return ""
	}
	return string(out)
}

// streamTranscript tails a JSONL transcript file and POSTs each line to the server.
func streamTranscript(path, sessionID, deviceID, project, relayID, server string) {
	// Wait for transcript file to appear (may not exist at SessionStart)
	var f *os.File
	for i := 0; i < 300; i++ { // up to 30 seconds
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", path)
		return
	}
	defer f.Close()

	// Seek to approximately the last 50 lines for backfill
	seekToLastLines(f, 50)

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			// Complete line (delimiter found) — safe to send
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				if !sendTranscriptLine(fullLine, sessionID, deviceID, project, relayID, server) {
					return // fatal error
				}
			}
		} else if line != "" {
			// Partial line (no newline yet) — buffer it
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// sendTranscriptLine POSTs a single transcript line to the server.
// Returns false if the server returned a fatal error (4xx except 429).
func sendTranscriptLine(line, sessionID, deviceID, project, relayID, server string) bool {
	// The line is valid JSON — embed it as raw JSON in the data field.
	// We build the JSON manually to avoid double-encoding the transcript line.
	payloadJSON := fmt.Sprintf(
		`{"device_id":%q,"session_id":%q,"project":%q,"relay_id":%q,"data":%s}`,
		deviceID, sessionID, project, relayID, line,
	)

	resp, err := postRawJSON(server+"/transcript", []byte(payloadJSON), 5*time.Second)
	if err != nil {
		log.Printf("Transcript POST error: %v", err)
		return true // transient, keep going
	}
	defer resp.Body.Close()

	code := resp.StatusCode
	if code >= 400 && code < 500 && code != 429 {
		log.Printf("Transcript POST fatal error: HTTP %d", code)
		return false
	}
	return true
}

// seekToLastLines positions the reader near the last N lines of the file.
func seekToLastLines(f *os.File, n int) {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return
	}

	// Read from the end, looking for newlines
	buf := make([]byte, 1)
	count := 0
	pos := info.Size() - 1

	for pos > 0 {
		f.Seek(pos, io.SeekStart)
		f.Read(buf)
		if buf[0] == '\n' {
			count++
			if count > n {
				f.Seek(pos+1, io.SeekStart)
				return
			}
		}
		pos--
	}

	// File has fewer than n lines — read from start
	f.Seek(0, io.SeekStart)
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
