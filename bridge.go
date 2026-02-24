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
	for {
		select {
		case <-done:
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if line != "" {
			line = trimNewline(line)
			if line != "" {
				// Wrap in transcript message: {"type":"transcript","data":<raw JSONL>}
				msg := fmt.Sprintf(`{"type":"transcript","data":%s}`, line)
				ws.SendText([]byte(msg))
			}
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

