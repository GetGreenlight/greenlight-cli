//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func runRegister(args []string) {
	if len(args) != 1 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight register <device-id>\n")
		os.Exit(1)
	}

	deviceID := args[0]
	if !uuidPattern.MatchString(deviceID) {
		fmt.Fprintf(os.Stderr, "Error: invalid device ID %q (expected UUID format)\n", deviceID)
		os.Exit(1)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	configDir := filepath.Join(home, ".greenlight")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot create %s: %v\n", configDir, err)
		os.Exit(1)
	}

	configPath := filepath.Join(configDir, "config")
	if err := os.WriteFile(configPath, []byte("device_id="+deviceID+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot write %s: %v\n", configPath, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Registered device %s\n", deviceID)
}
