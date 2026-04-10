//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// historyEntry is one permission request outcome stored on disk.
type historyEntry struct {
	RequestID   string          `json:"request_id"`
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input,omitempty"`
	Outcome     string          `json:"outcome"`
	Agent       string          `json:"agent,omitempty"`
	RespondedAt string          `json:"responded_at"`
}

const (
	historyMaxLines      = 1000
	historyTruncateLines = 500
)

// historyStorePath returns ~/.greenlight/history/
func historyStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".greenlight", "history")
}

// sanitizeProject replaces path separators and dots to produce a safe filename.
func sanitizeProject(project string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "_")
	return r.Replace(project)
}

// appendHistoryEntry appends a history entry to the project's JSONL file.
func appendHistoryEntry(project string, entry historyEntry) {
	dir := historyStorePath()
	if dir == "" {
		return
	}
	os.MkdirAll(dir, 0755)

	path := filepath.Join(dir, sanitizeProject(project)+".jsonl")
	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("history: marshal error: %v", err)
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("history: open error: %v", err)
		return
	}
	f.Write(line)
	f.Write([]byte("\n"))
	f.Close()

	// Truncate if file is too large
	maybeRotateHistory(path)
}

// maybeRotateHistory truncates the file to the most recent historyTruncateLines
// if the file exceeds historyMaxLines.
func maybeRotateHistory(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) <= historyMaxLines {
		return
	}

	// Keep most recent entries
	keep := lines[len(lines)-historyTruncateLines:]
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return
	}
	for _, l := range keep {
		out.Write([]byte(l))
		out.Write([]byte("\n"))
	}
	out.Close()
	os.Rename(tmp, path)
}

// listProjectHistory reads the last `limit` entries from a project's history file.
// Returns entries newest-first.
func listProjectHistory(project string, limit int) []historyEntry {
	dir := historyStorePath()
	if dir == "" {
		return nil
	}

	path := filepath.Join(dir, sanitizeProject(project)+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// Take last `limit` lines, reverse for newest-first
	start := 0
	if len(lines) > limit {
		start = len(lines) - limit
	}
	lines = lines[start:]

	entries := make([]historyEntry, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		var e historyEntry
		if json.Unmarshal([]byte(lines[i]), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}
