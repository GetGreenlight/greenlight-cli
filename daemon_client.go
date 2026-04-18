//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// connectViaDaemon is the thin client implementation of `greenlight connect`.
// It sends a connect request to the daemon, then enters raw mode and proxies
// terminal I/O over the IPC socket using binary framing.
func connectViaDaemon(agent, deviceID, project, cwd string) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Get terminal window size
	var winsize *ipcWinsize
	if ws, err := getWinsize(os.Stdin.Fd()); err == nil {
		winsize = &ipcWinsize{Rows: ws.Row, Cols: ws.Col}
	}

	// Forward client environment to the daemon so the child process
	// inherits vars like TERM, MOCK_CLAUDE_OUTPUT (tests), etc.
	clientEnv := make(map[string]string)
	for _, e := range os.Environ() {
		if k, v, ok := strings.Cut(e, "="); ok {
			clientEnv[k] = v
		}
	}

	// Send connect request
	req := ipcRequest{
		Type:     "connect",
		Agent:    agent,
		DeviceID: deviceID,
		Project:  project,
		Cwd:      cwd,
		Winsize:  winsize,
		Env:      clientEnv,
	}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to send connect request: %v\n", err)
		os.Exit(1)
	}

	// Read control response
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to read daemon response: %v\n", err)
		os.Exit(1)
	}

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: invalid daemon response: %v\n", err)
		os.Exit(1)
	}

	if resp.Type == "error" {
		fmt.Fprintf(os.Stderr, "greenlight: %s\n", resp.Message)
		os.Exit(1)
	}

	if resp.Type != "agent_instance_started" {
		fmt.Fprintf(os.Stderr, "greenlight: unexpected response: %s\n", resp.Type)
		os.Exit(1)
	}

	// Enter raw mode on the local terminal
	origTermios, err := setRawTerminal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to set raw mode: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		stdoutMu.Lock()
		resetScrollRegionLocked()
		stdoutMu.Unlock()
		restoreTerminal(origTermios)
	}()

	// Clear screen and reserve the bottom promptHeight rows for permission
	// prompts by setting a scroll region that confines agent output above them.
	if ws, err := getWinsize(os.Stdin.Fd()); err == nil && ws.Row > promptHeight {
		stdoutMu.Lock()
		fmt.Fprintf(os.Stdout, "\033[2J\033[H\033[1;%dr", ws.Row-promptHeight)
		stdoutMu.Unlock()
	}

	// Mutex for writing frames to the daemon connection. Multiple
	// goroutines write (stdin, signals, query responses) and writeFrame
	// does two Write() calls per frame (header + payload) which can
	// interleave without serialization.
	var connMu sync.Mutex

	// promptActive is set when a permission prompt is showing.
	// The stdin goroutine checks this to route keystrokes to promptResp
	// instead of stdin frames.
	var promptActive atomic.Bool

	// Cache last prompt content for redraw on resize.
	var lastPromptTool, lastPromptDetail string

	// Handle SIGWINCH — forward resize to daemon
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			if ws, err := getWinsize(os.Stdin.Fd()); err == nil {
				resizeData, _ := json.Marshal(ipcWinsize{Rows: ws.Row, Cols: ws.Col})
				connMu.Lock()
				writeFrame(conn, frameResize, resizeData)
				connMu.Unlock()
				// Update scroll region and redraw prompt under stdoutMu
				// to prevent interleaving with agent output frames.
				stdoutMu.Lock()
				if ws.Row > promptHeight {
					// DECSTBM moves cursor to home — save/restore around it.
					fmt.Fprintf(os.Stdout, "\0337\033[1;%dr\0338", ws.Row-promptHeight)
				}
				if promptActive.Load() {
					drawPromptLocked(lastPromptTool, lastPromptDetail)
				}
				stdoutMu.Unlock()
			}
		}
	}()

	// Handle SIGINT/SIGTERM — forward to daemon
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			sigName := "TERM"
			if sig == syscall.SIGINT {
				sigName = "INT"
			}
			sigData, _ := json.Marshal(map[string]string{"signal": sigName})
			connMu.Lock()
			writeFrame(conn, frameSignal, sigData)
			connMu.Unlock()
		}
	}()

	// Stdin → daemon (send user keystrokes as frameStdin, or framePromptResp during prompts)
	//
	// Terminal query responses (DA1 \033[?1;2c, OSC 10, etc.) arrive on
	// stdin as escape sequences. We buffer escape sequence bytes and only
	// forward or absorb when the sequence completes. This prevents partial
	// sequences from reaching the agent. We also filter prompt keystrokes
	// from escape sequences to prevent auto-approval.
	go func() {
		buf := make([]byte, 1)

		// Escape sequence state — tracked on every byte.
		inEscape := false
		inCSI := false
		inOSC := false
		prevWasEsc := false
		csiPrefix := byte(0)  // first byte after \033[ if ?/>/=
		oscCmd := 0            // OSC command number (10, 11, 12, etc.)
		oscCmdDone := false    // true once ';' seen (stop accumulating digits)
		var seqBuf []byte      // buffered bytes for current escape sequence

		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b := buf[0]

				// Track escape sequence state.
				inSeq := false
				seqDone := false    // sequence just completed this byte
				isResponse := false // this sequence is a terminal response

				if inCSI {
					inSeq = true
					if b >= 0x40 && b <= 0x7E { // final byte
						seqDone = true
						if b == 'c' && (csiPrefix == '?' || csiPrefix == '>') {
							isResponse = true
						}
						inCSI = false
						inEscape = false
					} else if csiPrefix == 0 && (b == '?' || b == '>' || b == '=') {
						csiPrefix = b
					}
				} else if inOSC {
					inSeq = true
					terminated := false
					if b == 0x07 { // BEL terminates OSC
						terminated = true
					} else if prevWasEsc && b == '\\' { // ESC \ (ST)
						terminated = true
					}
					prevWasEsc = (b == 0x1b)
					if terminated {
						seqDone = true
						if oscCmd == 10 || oscCmd == 11 || oscCmd == 12 {
							isResponse = true
						}
						inOSC = false
						inEscape = false
					} else {
						// Accumulate OSC command number (before ';')
						if !oscCmdDone && b >= '0' && b <= '9' {
							oscCmd = oscCmd*10 + int(b-'0')
						} else if b == ';' {
							oscCmdDone = true
						}
					}
				} else if inEscape {
					inSeq = true
					switch b {
					case '[':
						inCSI = true
						csiPrefix = 0
					case ']':
						inOSC = true
						oscCmd = 0
						oscCmdDone = false
					default:
						// Two-byte escape — sequence done
						seqDone = true
						inEscape = false
					}
				} else if b == 0x1b {
					inEscape = true
					prevWasEsc = false
					inSeq = true
					seqBuf = seqBuf[:0] // start new sequence
				}

				if inSeq {
					// Buffer escape sequence bytes
					seqBuf = append(seqBuf, b)

					if seqDone {
						if !isResponse {
							// Not a terminal response — forward buffered sequence
							connMu.Lock()
							writeFrame(conn, frameStdin, seqBuf)
							connMu.Unlock()
						}
						// Response sequences are silently dropped
						seqBuf = seqBuf[:0]
					}
				} else if promptActive.Load() && b >= '1' && b <= '4' {
					promptActive.Store(false)
					stdoutMu.Lock()
					clearPromptLocked()
					stdoutMu.Unlock()
					connMu.Lock()
					writeFrame(conn, framePromptResp, buf[:1])
					connMu.Unlock()
				} else {
					connMu.Lock()
					writeFrame(conn, frameStdin, buf[:n])
					connMu.Unlock()
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Daemon → stdout (read frames and handle them)
	exitCode := 0
	for {
		frameType, payload, err := readFrame(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("daemon client: read error: %v", err)
			}
			break
		}

		switch frameType {
		case frameStdout:
			stdoutMu.Lock()
			os.Stdout.Write(payload)
			stdoutMu.Unlock()

		case framePrompt:
			// Display permission prompt locally
			var prompt struct {
				ToolName string `json:"tool_name"`
				Detail   string `json:"detail"`
			}
			if json.Unmarshal(payload, &prompt) == nil {
				lastPromptTool = prompt.ToolName
				lastPromptDetail = prompt.Detail
				promptActive.Store(true)
				stdoutMu.Lock()
				drawPromptLocked(prompt.ToolName, prompt.Detail)
				stdoutMu.Unlock()
			}

		case framePromptCancel:
			// Server won the race — clear the prompt
			if promptActive.Load() {
				promptActive.Store(false)
				stdoutMu.Lock()
				clearPromptLocked()
				stdoutMu.Unlock()
			}

		case frameExit:
			var exit struct{ Code int `json:"code"` }
			json.Unmarshal(payload, &exit)
			exitCode = exit.Code
			goto done
		}
	}
done:

	signal.Stop(winchCh)
	signal.Stop(sigCh)

	// Restore terminal before exiting (defer handles the normal return path,
	// but os.Exit skips defers so we restore explicitly here too)
	stdoutMu.Lock()
	resetScrollRegionLocked()
	stdoutMu.Unlock()
	restoreTerminal(origTermios)

	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

const promptHeight uint16 = 4

// stdoutMu serializes writes to os.Stdout in the daemon client. The SIGWINCH
// goroutine, the output loop, and prompt drawing all write escape sequences
// and agent output to stdout — without serialization, interleaved writes
// corrupt the terminal display (e.g. scroll-region escapes spliced into
// agent output mid-stream).
var stdoutMu sync.Mutex

// drawPromptLocked draws a permission prompt in the reserved area below the
// scroll region. The scroll region is set once when the connect client
// starts and on resize, so this function only writes into the reserved rows.
// Caller must hold stdoutMu.
func drawPromptLocked(toolName, detail string) {
	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil || ws.Row < 10 {
		return
	}

	agentRows := ws.Row - promptHeight

	// Save cursor, draw prompt in the reserved area, then restore cursor.
	fmt.Fprintf(os.Stdout, "\0337") // DECSC
	sep := strings.Repeat("─", int(ws.Col))
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K\033[2m%s\033[0m", agentRows+1, sep)

	line := fmt.Sprintf(" \033[1m%s\033[0m: %s", toolName, detail)
	maxLen := int(ws.Col) - 1
	if len(toolName)+len(detail)+4 > maxLen {
		avail := maxLen - len(toolName) - 7
		if avail > 0 && avail < len(detail) {
			detail = detail[:avail] + "..."
		}
		line = fmt.Sprintf(" \033[1m%s\033[0m: %s", toolName, detail)
	}
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K%s", agentRows+2, line)
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K [1] Allow  [2] Always allow  [3] Deny  [4] Deny & stop", agentRows+3)
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K Choice: ", agentRows+4)
	fmt.Fprintf(os.Stdout, "\0338") // DECRC
}

// clearPromptLocked clears the reserved prompt area below the scroll region.
// The scroll region itself is not modified — it stays permanently set.
// Caller must hold stdoutMu.
func clearPromptLocked() {
	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil {
		return
	}
	agentRows := ws.Row - promptHeight
	fmt.Fprintf(os.Stdout, "\0337") // DECSC
	for i := uint16(0); i < promptHeight; i++ {
		fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K", agentRows+1+i)
	}
	fmt.Fprintf(os.Stdout, "\0338") // DECRC
}

// resetScrollRegionLocked moves the cursor below the agent scroll region,
// clears the reserved prompt area, and resets the scroll region to full
// terminal. This preserves the agent's exit output so it remains visible
// after greenlight exits. Caller must hold stdoutMu.
func resetScrollRegionLocked() {
	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintf(os.Stdout, "\033[r")
		return
	}
	agentRows := ws.Row - promptHeight
	// Clear the reserved prompt area.
	for i := uint16(0); i < promptHeight; i++ {
		fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K", agentRows+1+i)
	}
	// Move cursor to just below the agent area so the shell prompt
	// appears after the agent's output.
	fmt.Fprintf(os.Stdout, "\033[%d;1H", agentRows+1)
	// Reset scroll region.
	fmt.Fprintf(os.Stdout, "\033[r")
	// Re-position cursor after reset (some terminals move cursor on DECSTBM reset).
	fmt.Fprintf(os.Stdout, "\033[%d;1H", agentRows+1)
}

// setRawTerminal puts os.Stdin into raw mode and returns the original termios
// for later restoration.
func setRawTerminal() (*syscall.Termios, error) {
	fd := int(os.Stdin.Fd())
	var orig syscall.Termios

	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlReadTermios,
		uintptr(ptrOf(&orig)),
	); errno != 0 {
		return nil, errno
	}

	raw := orig
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag &^= syscall.PARENB | syscall.CSIZE
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON |
		syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlWriteTermios,
		uintptr(ptrOf(&raw)),
	); errno != 0 {
		return nil, errno
	}

	return &orig, nil
}

// restoreTerminal restores the terminal to its original settings.
func restoreTerminal(orig *syscall.Termios) {
	if orig == nil {
		return
	}
	fd := int(os.Stdin.Fd())
	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlWriteTermios,
		uintptr(ptrOf(orig)),
	)
}

