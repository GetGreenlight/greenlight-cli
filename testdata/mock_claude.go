// mock_claude is a minimal stand-in for the real claude binary.
// It prints a marker line so tests can verify the child was launched.
//
// Modes (controlled by env vars):
//
// MOCK_CLAUDE_OUTPUT — Read one line from stdin and write it to this file.
// Allows tests to verify input was injected into the subprocess via the PTY.
//
// MOCK_CLAUDE_TRANSCRIPT — Write test JSONL lines to this file path, then
// spawn `greenlight stream` to relay them through the bridge file. Allows
// tests to verify the full transcript pipeline (transcript → streamer →
// bridge → tailBridge → WS text frames).
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	fmt.Println("MOCK_CLAUDE_STARTED")

	if path := os.Getenv("MOCK_CLAUDE_OUTPUT"); path != "" {
		readStdinToFile(path)
		return
	}

	if path := os.Getenv("MOCK_CLAUDE_TRANSCRIPT"); path != "" {
		runTranscriptTest(path)
		return
	}
}

func readStdinToFile(outputPath string) {
	lineCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	select {
	case line := <-lineCh:
		os.WriteFile(outputPath, []byte(line), 0644)
	case <-time.After(10 * time.Second):
		os.WriteFile(outputPath, []byte("TIMEOUT: no input received"), 0644)
	}
}

func runTranscriptTest(transcriptPath string) {
	bridgePath := os.Getenv("GREENLIGHT_BRIDGE")
	sessionID := os.Getenv("GREENLIGHT_SESSION_ID")

	if bridgePath == "" || sessionID == "" {
		fmt.Fprintf(os.Stderr, "GREENLIGHT_BRIDGE and GREENLIGHT_SESSION_ID required\n")
		os.Exit(1)
	}

	// Write test transcript JSONL lines
	f, err := os.Create(transcriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create transcript: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":"TRANSCRIPT_TEST_LINE_1"}`)
	fmt.Fprintln(f, `{"type":"assistant","message":"TRANSCRIPT_TEST_LINE_2"}`)
	f.Close()

	// Spawn greenlight stream to tail the transcript and write to bridge.
	// greenlight is on PATH (same temp dir as this binary).
	cmd := exec.Command("greenlight", "stream",
		"--transcript", transcriptPath,
		"--session-id", sessionID,
		"--relay-id", sessionID,
		"--bridge", bridgePath,
	)
	cmd.Stdout = os.Stderr // don't pollute stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start streamer: %v\n", err)
		os.Exit(1)
	}

	// Give the streamer time to process the lines through:
	// transcript file → streamer → bridge file → tailBridge → WS
	time.Sleep(2 * time.Second)

	cmd.Process.Kill()
	cmd.Wait()
}
