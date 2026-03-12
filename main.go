//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// version is set at build time via -ldflags "-X main.version=..."
var version string

// wsURL is the relay server URL, set at build time via:
//
//	go build -ldflags "-X main.wsURL=wss://api.aigreenlight.app/ws/relay" -o greenlight .
var wsURL string

func main() {
	// Log to file to avoid polluting the terminal (which may be in raw mode)
	if logPath := os.Getenv("GREENLIGHT_LOG"); logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(f)
		}
	} else {
		logPath = filepath.Join(os.TempDir(), fmt.Sprintf("greenlight-%d.log", os.Getpid()))
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(f)
		}
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "connect":
		runConnect(os.Args[2:])
	case "hook":
		runHook(os.Args[2:])
	case "stream":
		runStream(os.Args[2:])
	case "register":
		runRegister(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "version", "--version", "-v":
		printVersion()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "greenlight: unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
	}
	if wsURL != "" {
		return fmt.Sprintf("greenlight %s (relay: %s)", v, wsURL)
	}
	return fmt.Sprintf("greenlight %s", v)
}

func printVersion() {
	fmt.Fprintln(os.Stderr, versionString())
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `%s

Usage: greenlight <command> [flags]

Commands:
  connect    Start an agent session with a remote relay to the Greenlight app
  register   Register a device ID for the Greenlight app
  agent      Get or set the default agent runtime (claude, codex, copilot, cursor, pi)
  hook       Handle agent hook events (used by hooks, not called directly)
  version    Print version and build settings

Run 'greenlight <command> --help' for details on a command.
`, versionString())
}
