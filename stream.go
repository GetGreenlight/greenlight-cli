//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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
		if agent == "gemini" {
			streamGeminiBridge(*transcriptPath, *bridge)
		} else {
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
	sent := 0
	var lastSize int64
	var lastMsgOffset int64 // byte offset of the last sent message's opening '{'

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

		if sessionID == "" {
			// First read: full parse
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
				time.Sleep(200 * time.Millisecond)
				continue
			}

			sessionID = transcript.SessionID
			for i := sent; i < len(transcript.Messages); i++ {
				writeGeminiMessage(bridge, transcript.Messages[i], sessionID)
			}
			sent = len(transcript.Messages)
			lastMsgOffset = findLastMsgOffset(data, sent)
		} else {
			// Incremental read: read from last message offset
			f, err := os.Open(transcriptPath)
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}

			f.Seek(lastMsgOffset, io.SeekStart)
			tail, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}

			// tail starts at the last sent message's '{'.
			// Find the end of that message (skip it), then parse remaining
			// messages. We wrap in '[' ... ']' to make a valid JSON array.
			//
			// Structure: {last_msg}, {new_msg1}, {new_msg2} ]\n  "kind":...}
			// We need to: strip from the ']' that closes messages array onward,
			// then prepend '[' to get a parseable array.

			// Find the ']' that closes the messages array by scanning backwards
			// from end, skipping whitespace and trailing object fields
			closeIdx := findMessagesClose(tail)
			if closeIdx < 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}

			arrayContent := append([]byte{'['}, tail[:closeIdx]...)
			arrayContent = append(arrayContent, ']')

			var messages []json.RawMessage
			if err := json.Unmarshal(arrayContent, &messages); err != nil {
				// Mid-write; retry
				time.Sleep(200 * time.Millisecond)
				continue
			}

			// First message is the last already-sent one (our anchor); skip it
			for i := 1; i < len(messages); i++ {
				writeGeminiMessage(bridge, messages[i], sessionID)
			}
			sent += len(messages) - 1

			// Update offset: re-read full file size to find new last message position
			if len(messages) > 1 {
				if data, err := os.ReadFile(transcriptPath); err == nil {
					lastMsgOffset = findLastMsgOffset(data, sent)
				}
			}
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// writeGeminiMessage transforms a gemini message and writes it to the bridge.
func writeGeminiMessage(bridge *os.File, raw json.RawMessage, sessionID string) {
	transformed := transformGeminiMessage(raw, sessionID)
	if transformed == nil {
		return
	}
	compactBytes, err := json.Marshal(transformed)
	if err != nil {
		log.Printf("gemini message marshal error: %v", err)
		return
	}
	if _, werr := fmt.Fprintln(bridge, string(compactBytes)); werr != nil {
		log.Printf("Bridge write error: %v", werr)
	}
}

// findLastMsgOffset finds the byte offset of the opening '{' of the nth message
// (0-indexed count = sent) in the messages array. Falls back to 0 if not found.
func findLastMsgOffset(data []byte, sent int) int64 {
	if sent == 0 {
		return 0
	}

	// Find "messages" key, then count message objects
	idx := bytes.Index(data, []byte(`"messages"`))
	if idx < 0 {
		return 0
	}

	// Find the opening '[' of the messages array
	arrayStart := bytes.IndexByte(data[idx:], '[')
	if arrayStart < 0 {
		return 0
	}
	pos := idx + arrayStart + 1

	// Count through message objects by tracking brace depth
	count := 0
	lastObjStart := int64(0)
	depth := 0
	inString := false
	escape := false

	for i := pos; i < len(data); i++ {
		if escape {
			escape = false
			continue
		}
		c := data[i]
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' {
			if depth == 0 {
				if count == sent-1 {
					lastObjStart = int64(i)
					return lastObjStart
				}
			}
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				count++
			}
		} else if c == ']' && depth == 0 {
			break
		}
	}

	return lastObjStart
}

// findMessagesClose finds the index of the ']' that closes the messages array
// in a byte slice that starts partway through the array.
func findMessagesClose(data []byte) int {
	depth := 0
	inString := false
	escape := false

	for i := 0; i < len(data); i++ {
		if escape {
			escape = false
			continue
		}
		c := data[i]
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' || c == '[' {
			depth++
		} else if c == '}' {
			depth--
		} else if c == ']' {
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// geminiMessage is the structure of a message in a Gemini transcript.
type geminiMessage struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	Model     string          `json:"model"`
	Tokens    *geminiTokens   `json:"tokens,omitempty"`
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
func transformGeminiMessage(raw json.RawMessage, sessionID string) map[string]interface{} {
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
		return map[string]interface{}{
			"type":      "user",
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message": map[string]interface{}{
				"role":    "user",
				"content": text,
			},
		}

	case "gemini":
		// Gemini assistant content is a string
		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			return nil
		}
		entry := map[string]interface{}{
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
			entry["message"].(map[string]interface{})["usage"] = map[string]interface{}{
				"input_tokens":  msg.Tokens.Input,
				"output_tokens": msg.Tokens.Output,
				"cache_read_input_tokens": msg.Tokens.Cached,
			}
		}
		return entry

	default:
		// Tool use or other types — pass through with minimal wrapping
		var content interface{}
		json.Unmarshal(raw, &content)
		return map[string]interface{}{
			"type":      msg.Type,
			"uuid":      msg.ID,
			"timestamp": msg.Timestamp,
			"sessionId": sessionID,
			"message":   content,
		}
	}
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
