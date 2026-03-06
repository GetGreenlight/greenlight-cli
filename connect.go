//go:build darwin || linux

package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

func runConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	resume := fs.String("resume", "", "Resume a previous session by ID")
	deviceID := fs.String("device-id", "", "Device ID (overrides GREENLIGHT_DEVICE_ID env and config file)")
	project := fs.String("project", "", "Project name (overrides GREENLIGHT_PROJECT env and config file)")
	agentFlag := fs.String("agent", "", "Agent runtime: claude or cursor (overrides GREENLIGHT_AGENT env and config file)")
	fs.Parse(args)

	if wsURL == "" {
		fmt.Fprintf(os.Stderr, "greenlight: no relay server URL configured (binary must be built with -ldflags)\n")
		os.Exit(1)
	}

	// Resolve agent runtime: flag > env > config > default
	agent := resolveAgent(*agentFlag)
	if !knownAgents[agent] {
		fmt.Fprintf(os.Stderr, "greenlight: unknown agent %q (supported: claude, cursor, gemini)\n", agent)
		os.Exit(1)
	}

	// Build the agent command
	command := agentBinary(agent)
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
	if devID == "" {
		fmt.Fprintf(os.Stderr, "greenlight: device ID is required (use --device-id, GREENLIGHT_DEVICE_ID, or set device_id in ~/.greenlight/config)\n")
		fmt.Fprintf(os.Stderr, "greenlight: your device ID can be found on the About tab in the Greenlight app\n")
		os.Exit(1)
	}

	// Resolve project: flag > env > config file (required)
	proj := *project
	if proj == "" {
		proj = os.Getenv("GREENLIGHT_PROJECT")
	}
	if proj == "" {
		proj = readConfigValue("project")
	}
	if proj == "" {
		fmt.Fprintf(os.Stderr, "greenlight: project name is required (use --project)\n")
		os.Exit(1)
	}

	// Reuse relay ID for resumed conversations so the phone sees the same session
	var relayID string
	if *resume != "" {
		relayID = lookupRelayID(*resume)
	}
	if relayID == "" {
		relayID = generateUUID()
	}
	dialURL := wsURL
	u, err := url.Parse(dialURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: bad relay URL: %v\n", err)
		os.Exit(1)
	}
	q := u.Query()
	q.Set("relay_id", relayID)
	q.Set("project", proj)
	q.Set("agent", agentServerName(agent))
	if version != "" {
		q.Set("version", version)
	}
	u.RawQuery = q.Encode()
	dialURL = u.String()

	// Derive HTTP base URL for enrollment
	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	// Enroll session with the relay server
	if err := enrollSession(baseURL, devID, relayID, proj); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: session enrollment failed: %v\n", err)
		os.Exit(1)
	}

	// Install Claude Code hooks
	if err := installHooks(agent); err != nil {
		log.Printf("Warning: failed to install hooks: %v", err)
	}

	// Create bridge file for transcript relay
	bridgePath := filepath.Join(os.TempDir(), "greenlight-bridge-"+relayID)
	if f, err := os.Create(bridgePath); err == nil {
		f.Close()
	}
	defer os.Remove(bridgePath)

	// Export greenlight vars into the child process
	exportEnvs := map[string]string{
		"GREENLIGHT_DEVICE_ID":  devID,
		"GREENLIGHT_SESSION_ID": relayID,
		"GREENLIGHT_PROJECT":    proj,
		"GREENLIGHT_BRIDGE":     bridgePath,
		"GREENLIGHT_AGENT":      agent,
	}

	r, err := New(command, cmdArgs, dialURL, devID, WSModeRW, exportEnvs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	// Set kill function for remote "pull the plug"
	if r.ws != nil {
		r.ws.killFunc = func() {
			if r.cmd.Process != nil {
				r.killed = true
				syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}

	// Start bridge tailer — sends transcript lines from bridge file over WebSocket
	var bridgeDone chan struct{}
	var bridgeFinished chan struct{}
	if r.ws != nil {
		bridgeDone = make(chan struct{})
		bridgeFinished = make(chan struct{})
		go func() {
			tailBridge(bridgePath, r.ws, bridgeDone)
			close(bridgeFinished)
		}()
	}

	runErr := r.Run()

	// Signal bridge tailer to drain remaining lines and wait for it
	// to finish. This must happen before closing the WebSocket.
	if bridgeDone != nil {
		close(bridgeDone)
		<-bridgeFinished
	}

	r.CloseWS()

	// If the session was killed remotely, print resume instructions
	if r.killed {
		if convID := lookupConversationID(relayID); convID != "" {
			fmt.Fprintf(os.Stderr, "\nTo resume this conversation use --resume %s\n", convID)
		}
	}

	if runErr != nil {
		os.Exit(1)
	}
}

func generateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
