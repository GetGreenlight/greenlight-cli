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
//	go build -ldflags "-X main.wsURL=wss://permit.dnmfarrell.com/ws/relay" -o greenlight .
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

func printVersion() {
	v := version
	if v == "" {
		v = "dev"
	}
	fmt.Fprintf(os.Stderr, "greenlight %s (relay: %s)\n", v, wsURL)
}

func printUsage() {
	v := version
	if v == "" {
		v = "dev"
	}
	fmt.Fprintf(os.Stderr, `greenlight %s (relay: %s)

Usage: greenlight <command> [flags]

Commands:
  connect    Start Claude Code with a remote relay to the Greenlight app
  hook       Handle Claude Code hook events (used by hooks, not called directly)
  version    Print version and build settings

Run 'greenlight <command> --help' for details on a command.
`, v, wsURL)
}
