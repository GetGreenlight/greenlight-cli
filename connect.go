//go:build darwin || linux

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

func runConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	resume := fs.String("resume", "", "Resume a previous Claude Code session by ID")
	deviceID := fs.String("device-id", "", "Device ID (overrides GREENLIGHT_DEVICE_ID env and config file)")
	project := fs.String("project", "", "Project name (overrides GREENLIGHT_PROJECT env and config file)")
	fs.Parse(args)

	if wsURL == "" {
		fmt.Fprintf(os.Stderr, "greenlight: no relay server URL configured (binary must be built with -ldflags)\n")
		os.Exit(1)
	}

	// Build the claude command
	command := "claude"
	var cmdArgs []string
	if *resume != "" {
		cmdArgs = append(cmdArgs, "--resume", *resume)
	}

	// Resolve device ID: flag > env > config file
	devID := *deviceID
	if devID == "" {
		devID = os.Getenv("GREENLIGHT_DEVICE_ID")
	}
	if devID == "" {
		devID = readConfigValue("device_id")
	}

	// Generate a relay ID and append it to the WebSocket URL
	relayID := generateUUID()
	dialURL := wsURL
	u, err := url.Parse(dialURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: bad relay URL: %v\n", err)
		os.Exit(1)
	}
	q := u.Query()
	q.Set("relay_id", relayID)
	u.RawQuery = q.Encode()
	dialURL = u.String()

	// Enroll session with the relay server
	if err := enrollSession(dialURL, devID, relayID); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: session enrollment failed: %v\n", err)
		os.Exit(1)
	}

	// Resolve project: flag > env > config file
	proj := *project
	if proj == "" {
		proj = os.Getenv("GREENLIGHT_PROJECT")
	}
	if proj == "" {
		proj = readConfigValue("project")
	}

	// Export greenlight vars into the child process
	exportEnvs := map[string]string{
		"GREENLIGHT_DEVICE_ID":  devID,
		"GREENLIGHT_SESSION_ID": relayID,
		"GREENLIGHT_PROJECT":    proj,
	}

	r, err := New(command, cmdArgs, dialURL, devID, WSModeR, exportEnvs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	if err := r.Run(); err != nil {
		os.Exit(1)
	}
}

// enrollSession registers this session with the Greenlight server and blocks
// until the user approves it on their phone. Returns an error if rejected or timed out.
func enrollSession(wsDialURL, deviceID, sessionID string) error {
	u, err := url.Parse(wsDialURL)
	if err != nil {
		return fmt.Errorf("bad WebSocket URL: %w", err)
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	enrollURL := fmt.Sprintf("%s://%s/session/enroll", scheme, u.Host)

	body, err := json.Marshal(map[string]string{
		"device_id":  deviceID,
		"session_id": sessionID,
	})
	if err != nil {
		return fmt.Errorf("failed to encode request: %w", err)
	}

	client := &http.Client{Timeout: 65 * time.Second}
	resp, err := client.Post(enrollURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("enrollment request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("enrollment rejected (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		Approved bool   `json:"approved"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	if !result.Approved {
		if result.Message != "" {
			return fmt.Errorf("session enrollment %s", result.Message)
		}
		return fmt.Errorf("session enrollment rejected")
	}

	log.Printf("Session %s enrolled successfully", sessionID)
	return nil
}

func generateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
