//go:build darwin || linux

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

func runStream(args []string) {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	transcriptPath := fs.String("transcript", "", "Path to transcript JSONL file")
	sessionID := fs.String("session-id", "", "Claude session ID")
	deviceID := fs.String("device-id", "", "Device ID")
	project := fs.String("project", "", "Project name")
	relayID := fs.String("relay-id", "", "Relay ID")
	server := fs.String("server", "", "Server base URL")
	bridge := fs.String("bridge", "", "Bridge file path (write lines here instead of HTTP POST)")
	fs.Parse(args)

	if *transcriptPath == "" || *sessionID == "" {
		fmt.Fprintf(os.Stderr, "greenlight stream: missing required flags\n")
		os.Exit(1)
	}

	// Bridge mode: server and device-id are not required
	if *bridge == "" && (*deviceID == "" || *server == "") {
		fmt.Fprintf(os.Stderr, "greenlight stream: missing required flags (--server, --device-id or --bridge)\n")
		os.Exit(1)
	}

	// Write PID file for the hook to check
	pidFile := filepath.Join(os.TempDir(), "greenlight-stream-"+*sessionID+".pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %s", os.Getpid(), *relayID)), 0644)
	defer os.Remove(pidFile)

	if *bridge != "" {
		streamToBridge(*transcriptPath, *sessionID, *bridge)
	} else {
		streamTranscript(*transcriptPath, *sessionID, *deviceID, *project, *relayID, *server)
	}
}

// streamToBridge tails a JSONL transcript file and appends each line to the bridge file.
// The bridge file is tailed by `connect` which sends lines over the relay WebSocket.
func streamToBridge(transcriptPath, sessionID, bridgePath string) {
	// Wait for transcript file to appear (may not exist at SessionStart)
	var f *os.File
	for i := 0; i < 300; i++ { // up to 30 seconds
		var err error
		f, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", transcriptPath)
		return
	}
	defer f.Close()

	// Start from beginning — transcript file is fresh for each session.
	// No seekToLastLines backfill needed, which avoids duplicates if
	// a second streamer is accidentally spawned.

	bridge, err := os.OpenFile(bridgePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open bridge file: %v", err)
		return
	}
	defer bridge.Close()

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			// Complete line (delimiter found) — safe to write
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				// Write the raw JSONL line to the bridge file (one line per entry)
				if _, werr := fmt.Fprintln(bridge, fullLine); werr != nil {
					log.Printf("Bridge write error: %v", werr)
					return
				}
			}
		} else if line != "" {
			// Partial line (no newline yet) — buffer it
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// streamTranscript tails a JSONL transcript file and POSTs each line to the server.
func streamTranscript(path, sessionID, deviceID, project, relayID, server string) {
	// Wait for transcript file to appear (may not exist at SessionStart)
	var f *os.File
	for i := 0; i < 300; i++ { // up to 30 seconds
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		log.Printf("Transcript file never appeared: %s", path)
		return
	}
	defer f.Close()

	// Seek to approximately the last 50 lines for backfill
	seekToLastLines(f, 50)

	reader := bufio.NewReader(f)
	var partial string

	for {
		line, err := reader.ReadString('\n')
		if err == nil {
			// Complete line (delimiter found) — safe to send
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				if !sendTranscriptLine(fullLine, sessionID, deviceID, project, relayID, server) {
					return // fatal error
				}
			}
		} else if line != "" {
			// Partial line (no newline yet) — buffer it
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Transcript read error: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// sendTranscriptLine POSTs a single transcript line to the server.
// Returns false if the server returned a fatal error (4xx except 429).
func sendTranscriptLine(line, sessionID, deviceID, project, relayID, server string) bool {
	// The line is valid JSON — embed it as raw JSON in the data field.
	// We build the JSON manually to avoid double-encoding the transcript line.
	payloadJSON := fmt.Sprintf(
		`{"device_id":%q,"session_id":%q,"project":%q,"relay_id":%q,"data":%s}`,
		deviceID, sessionID, project, relayID, line,
	)

	resp, err := postRawJSON(server+"/transcript", []byte(payloadJSON), 5*time.Second)
	if err != nil {
		log.Printf("Transcript POST error: %v", err)
		return true // transient, keep going
	}
	defer resp.Body.Close()

	code := resp.StatusCode
	if code >= 400 && code < 500 && code != 429 {
		log.Printf("Transcript POST fatal error: HTTP %d", code)
		return false
	}
	return true
}

// seekToLastLines positions the reader near the last N lines of the file.
func seekToLastLines(f *os.File, n int) {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return
	}

	// Read from the end, looking for newlines
	buf := make([]byte, 1)
	count := 0
	pos := info.Size() - 1

	for pos > 0 {
		f.Seek(pos, io.SeekStart)
		f.Read(buf)
		if buf[0] == '\n' {
			count++
			if count > n {
				f.Seek(pos+1, io.SeekStart)
				return
			}
		}
		pos--
	}

	// File has fewer than n lines — read from start
	f.Seek(0, io.SeekStart)
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
