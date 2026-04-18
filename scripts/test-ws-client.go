// test-ws-client connects to the server's device WebSocket (simulating a phone)
// and sends commands to test daemon features.
//
// Usage:
//   go run scripts/test-ws-client.go -device-id <id> [-server ws://localhost:8080] <command> [args...]
//
// Commands:
//   listen                     - connect and print all messages (for debugging)

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"nhooyr.io/websocket"
)

func main() {
	deviceID := flag.String("device-id", "", "Device ID")
	secret := flag.String("secret", "", "Device secret")
	server := flag.String("server", "ws://localhost:8080", "Server WebSocket URL")
	flag.Parse()

	if *deviceID == "" {
		fmt.Fprintf(os.Stderr, "Usage: go run test-ws-client.go -device-id <id> <command> [args...]\n")
		os.Exit(1)
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Commands: listen\n")
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/ws?device_id=%s&secret=%s", *server, *deviceID, *secret)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	switch args[0] {
	case "listen":
		listen(ctx, conn)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", args[0])
		os.Exit(1)
	}
}

func listen(ctx context.Context, conn *websocket.Conn) {
	fmt.Fprintf(os.Stderr, "Listening for messages (Ctrl+C to stop)...\n")
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Disconnected: %v\n", err)
			return
		}
		var pretty map[string]interface{}
		if json.Unmarshal(data, &pretty) == nil {
			out, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Println(string(out))
		} else {
			fmt.Println(string(data))
		}
	}
}
