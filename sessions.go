//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// sessionsFilePath returns the path to ~/.greenlight/sessions.json.
func sessionsFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".greenlight", "sessions.json")
}

// loadSessions reads the conversation_id → relay_id mapping from disk.
func loadSessions() map[string]string {
	path := sessionsFilePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

// lookupRelayID returns the stored relay_id for a conversation, or "".
func lookupRelayID(conversationID string) string {
	m := loadSessions()
	if m == nil {
		return ""
	}
	return m[conversationID]
}

// saveRelayID persists a conversation_id → relay_id mapping.
func saveRelayID(conversationID, relayID string) {
	path := sessionsFilePath()
	if path == "" {
		return
	}
	m := loadSessions()
	if m == nil {
		m = make(map[string]string)
	}
	m[conversationID] = relayID

	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, data, 0644)
}
