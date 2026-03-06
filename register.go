//go:build darwin || linux

package main

import (
	"fmt"
	"os"
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

	if err := writeConfigValue("device_id", deviceID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Registered device %s\n", deviceID)
}
