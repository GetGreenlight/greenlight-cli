//go:build darwin || linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

// tailBridge tails the bridge file and sends each line over the WebSocket
// as a JSON transcript message. Blocks until done is closed or an error occurs.
// After done is closed, drains any remaining lines before returning.
func tailBridge(path string, ws *WSClient, done <-chan struct{}) {
	// Wait for the bridge file to appear (hook creates it)
	var f *os.File
	for {
		select {
		case <-done:
			return
		default:
		}
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer f.Close()

	// Seek to end — no backfill, fresh session
	f.Seek(0, io.SeekEnd)

	reader := bufio.NewReader(f)
	var partial string
	stopping := false
	for {
		if stopping {
			// Drain pass: give the streamer a moment to finish writing,
			// then read and send all remaining complete lines.
			time.Sleep(500 * time.Millisecond)
			for {
				line, err := reader.ReadString('\n')
				if err == nil {
					// Complete line (delimiter found)
					fullLine := trimNewline(partial + line)
					partial = ""
					if fullLine != "" {
						msg := fmt.Sprintf(`{"type":"transcript","data":%s}`, fullLine)
						ws.SendText([]byte(msg))
					}
				} else {
					// EOF or error — send any remaining buffered partial
					if partial != "" {
						msg := fmt.Sprintf(`{"type":"transcript","data":%s}`, partial)
						ws.SendText([]byte(msg))
					}
					return
				}
			}
		}

		select {
		case <-done:
			stopping = true
			continue
		default:
		}

		line, err := reader.ReadString('\n')
		if err == nil {
			// Complete line (delimiter found) — safe to send
			fullLine := trimNewline(partial + line)
			partial = ""
			if fullLine != "" {
				msg := fmt.Sprintf(`{"type":"transcript","data":%s}`, fullLine)
				ws.SendText([]byte(msg))
			}
		} else if line != "" {
			// Partial line (no newline yet) — buffer it
			partial += line
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("bridge: read error: %v", err)
				return
			}
			// EOF — wait for more data
			time.Sleep(100 * time.Millisecond)
		}
	}
}

