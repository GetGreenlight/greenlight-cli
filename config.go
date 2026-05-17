//go:build darwin || linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// configPath returns the path to ~/.greenlight/config.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".greenlight", "config"), nil
}

// readConfigValue reads a value by key from ~/.greenlight/config.
// The config file uses simple key=value format, one per line.
// Returns empty string if the file doesn't exist or the key is not found.
func readConfigValue(key string) string {
	path, err := configPath()
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseConfigValue(f, key)
}

// parseConfigValue is the pure parsing core of readConfigValue: it scans
// key=value lines from r and returns the value for key, or "" if absent.
// Blank lines and #-comments are skipped. Separated out so it can be fuzzed
// without touching the filesystem.
func parseConfigValue(r io.Reader, key string) string {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeConfigValue upserts a key=value pair in ~/.greenlight/config,
// preserving all other entries.
func writeConfigValue(key, value string) error {
	path, err := configPath()
	if err != nil {
		return fmt.Errorf("cannot determine config path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create config dir: %w", err)
	}

	// Read existing lines
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
	}

	// Upsert the key
	found := false
	entry := key + "=" + value
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, _, ok := strings.Cut(trimmed, "=")
		if ok && strings.TrimSpace(k) == key {
			lines[i] = entry
			found = true
			break
		}
	}
	if !found {
		// Remove trailing empty lines, append, and re-add newline
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, entry)
	}

	output := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(output), 0644)
}
