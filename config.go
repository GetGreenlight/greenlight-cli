//go:build darwin || linux

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// readConfigValue reads a value by key from ~/.greenlight/config.
// The config file uses simple key=value format, one per line.
// Returns empty string if the file doesn't exist or the key is not found.
func readConfigValue(key string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(home, ".greenlight", "config"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
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
