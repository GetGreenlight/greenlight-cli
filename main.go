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

// updateURL overrides the binary download URL for updates, set at build time via:
//
//	go build -ldflags "-X main.updateURL=file:///path/to/binary" -o greenlight .
//
// When set, skips GitHub version checking and downloads from this URL directly.
// Useful for local testing of the update flow.
var updateURL string

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
		runConnect(nil)
		return
	}

	switch os.Args[1] {
	case "connect":
		runConnect(os.Args[2:])
	case "stream":
		runStream(os.Args[2:])
	case "register":
		runRegister(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "daemon":
		runDaemon(os.Args[2:])
	case "household":
		runHousehold(os.Args[2:])
	case "wd":
		runWD(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "version", "--version", "-v":
		printVersion()
	case "help", "--help", "-h":
		printUsage()
	default:
		// If it looks like a subcommand (no leading dash), it's an error.
		// Otherwise fall through to connect for flags like --agent.
		if len(os.Args[1]) > 0 && os.Args[1][0] != '-' {
			fmt.Fprintf(os.Stderr, "greenlight: unknown command %q\nRun 'greenlight help' for usage.\n", os.Args[1])
			os.Exit(1)
		}
		runConnect(os.Args[1:])
	}
}

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
	}
	return fmt.Sprintf("greenlight %s", v)
}

func printVersion() {
	fmt.Fprintln(os.Stderr, versionString())
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `%s

Usage: greenlight [flags]
       greenlight <command> [flags]

When no command is given, 'connect' is used by default.

Commands:
  connect    Start an agent session with a remote relay to the Greenlight app
  register   Register a device ID for the Greenlight app
  agent      Get or set the default agent runtime (claude, codex, copilot, cursor, gemini, pi)
  daemon     Manage the background daemon (start, stop, status)
  household  Manage multi-agent household entities (org, wd, job, pos, agent, model)
  wd         Manage working directories (create, list)
  update     Update greenlight to the latest version
  version    Print version and build settings

Run 'greenlight <command> --help' for details on a command.
`, versionString())
}
