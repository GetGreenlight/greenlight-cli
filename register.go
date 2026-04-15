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
		fmt.Fprintf(os.Stderr, "Usage: greenlight register <email>\n")
		os.Exit(1)
	}

	email := args[0]

	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Register user by email, get back user_id and the user's primary org_id.
	userID, orgID, err := registerUser(baseURL, email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := writeConfigValue("user_id", userID); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving user_id: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Registered user %s\n", userID)

	if orgID != "" {
		if err := writeConfigValue("organization_id", orgID); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving organization_id: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Working organization set to %s\n", orgID)
	}

	// Register host with the new user_id
	hostID := readConfigValue("host_id")
	if hostID == "" {
		hostID = generateUUID()
	}
	hostname, _ := os.Hostname()

	ioDeviceID, err := registerHost(baseURL, userID, hostID, hostname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error registering host: %v\n", err)
		os.Exit(1)
	}

	if err := writeConfigValue("host_id", hostID); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving host_id: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Registered host %s\n", hostID)

	if ioDeviceID != "" {
		if err := writeConfigValue("io_device_id", ioDeviceID); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving io_device_id: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Registered io_device %s\n", ioDeviceID)
	}
}
