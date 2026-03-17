//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/vt"
)

// promptData holds the pre-formatted prompt content for the render loop.
type promptData struct {
	separator string
	toolLine  string
	choices   string
	input     string
	height    uint16
}

// Relay holds the state for a running PTY relay session.
//
// It operates in two modes:
//
//   - Direct mode (normal): agent PTY output goes straight to stdout, giving
//     zero-latency passthrough identical to running the agent without greenlight.
//     The vt emulator receives the same output as a shadow copy.
//
//   - Render mode (during prompts): direct stdout writes stop. The render loop
//     paints the agent area from the vt emulator's screen buffer with the
//     permission prompt below it. Agent output continues flowing to the vt
//     emulator so the display stays live.
type Relay struct {
	cmd         *exec.Cmd
	master      *os.File
	slave       *os.File
	origTermios syscall.Termios
	mu          sync.Mutex // serializes writes to master
	ws          *WSClient  // optional WebSocket client
	killed      bool       // true if the child was killed (not normal exit)

	// Virtual terminal emulator — shadow copy of agent output. Always
	// receives output regardless of mode. Used for rendering during prompts.
	vtEmu           *vt.SafeEmulator
	vtCursorVisible atomic.Bool // mirrors the emulator's cursor visibility
	renderNeeded    atomic.Bool
	renderDone      chan struct{}

	// Shutdown coordination — closed when the child process exits.
	shutdownCh chan struct{}

	// Terminal permission prompt support.
	// promptActive controls mode switching: when true, the master→stdout
	// goroutine stops writing to stdout and the render loop takes over.
	promptReady   atomic.Bool                // true once stdin goroutine is running
	promptMu      sync.Mutex                 // serializes prompts (one at a time)
	promptActive  atomic.Bool                // true = render mode, false = direct mode
	promptCh      chan byte                   // keystrokes redirected here during prompt
	promptContent atomic.Pointer[promptData] // prompt text for render loop
}

// New creates a new Relay that will run the given command inside a PTY.
// If wsURL is non-empty, a WebSocket client is created for remote I/O.
// exportEnvs are added to the child environment.
func New(command string, args []string, wsURL, wsToken string, wsMode WSMode, exportEnvs map[string]string) (*Relay, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("openPTY: %w", err)
	}

	cmd := exec.Command(command, args...)

	cmd.Env = os.Environ()
	for k, v := range exportEnvs {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	r := &Relay{
		cmd:        cmd,
		master:     master,
		slave:      slave,
		promptCh:   make(chan byte, 32),
		renderDone: make(chan struct{}),
		shutdownCh: make(chan struct{}),
	}

	if wsURL != "" {
		r.ws = NewWSClient(wsURL, wsToken, wsMode, r.Inject)
	}

	return r, nil
}

// Run starts the child process and enters the main relay loop.
// It blocks until the child exits.
func (r *Relay) Run() error {
	defer r.cleanup()

	// Copy outer terminal window size to inner PTY
	if err := r.syncWinsize(); err != nil {
		log.Printf("warn: syncWinsize: %v", err)
	}

	// Create virtual terminal emulator sized to the outer terminal.
	// This runs as a shadow copy during direct mode, and becomes the
	// authoritative screen source during render mode (prompts).
	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil {
		return fmt.Errorf("getWinsize: %w", err)
	}
	r.vtEmu = vt.NewSafeEmulator(int(ws.Col), int(ws.Row))
	r.vtCursorVisible.Store(true) // cursor starts visible (DECTCEM default)
	r.vtEmu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) {
			r.vtCursorVisible.Store(visible)
		},
	})

	// Put outer stdin into raw mode
	if err := r.setRaw(); err != nil {
		return fmt.Errorf("setRaw: %w", err)
	}

	// Start child process on the slave PTY
	r.cmd.Stdin = r.slave
	r.cmd.Stdout = r.slave
	r.cmd.Stderr = r.slave
	r.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    3, // fd index of slave in child (see ExtraFiles below)
	}
	// Pass slave as fd 3 so Setctty index is predictable
	r.cmd.ExtraFiles = []*os.File{r.slave}

	if err := r.cmd.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	// We no longer need the slave in the parent
	r.slave.Close()
	r.slave = nil

	// Start WebSocket client if configured
	if r.ws != nil {
		go r.ws.Run()
	}

	// Start render loop — only does work when promptActive is true
	go r.renderLoop()

	// Handle SIGWINCH — forward window resize to inner PTY and vt emulator
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			outerWs, err := getWinsize(os.Stdin.Fd())
			if err != nil {
				continue
			}

			if pd := r.promptContent.Load(); pd != nil {
				// Prompt active: agent gets reduced rows
				agentRows := outerWs.Row - pd.height
				if agentRows < 5 {
					agentRows = 5
				}
				r.vtEmu.Resize(int(outerWs.Col), int(agentRows))
				agentWs := &Winsize{Row: agentRows, Col: outerWs.Col, Xpixel: outerWs.Xpixel, Ypixel: outerWs.Ypixel}
				setWinsize(r.master.Fd(), agentWs)
				r.renderNeeded.Store(true)
			} else {
				// No prompt: keep vt emulator in sync, forward to PTY
				r.vtEmu.Resize(int(outerWs.Col), int(outerWs.Row))
				setWinsize(r.master.Fd(), outerWs)
			}
		}
	}()

	// Handle SIGINT/SIGTERM — forward to child process group
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if r.cmd.Process != nil {
				r.cmd.Process.Signal(sig)
			}
		}
	}()

	// Relay loop
	done := make(chan error, 1)

	// master → stdout + vt emulator (dual mode)
	//
	// Direct mode (promptActive=false): write to stdout AND vt emulator.
	//   stdout gives zero-latency display, vt emulator keeps a shadow copy.
	//
	// Render mode (promptActive=true): write to vt emulator only.
	//   The render loop paints the screen from the vt emulator.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.master.Read(buf)
			if n > 0 {
				// Always feed shadow copy
				r.vtEmu.Write(buf[:n])

				if r.promptActive.Load() {
					// Render mode: let the render loop paint it
					r.renderNeeded.Store(true)
				} else {
					// Direct mode: straight to terminal
					os.Stdout.Write(buf[:n])
				}

				if r.ws != nil {
					r.ws.Send(buf[:n])
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// vt emulator → master (terminal query responses)
	//
	// The vt emulator generates responses to terminal queries (CSI 6 n,
	// CSI c, etc.). During direct mode, the physical terminal handles these
	// queries so we discard the vt responses. During render mode, the vt
	// emulator is the authoritative terminal so we relay responses back.
	//
	// We must always read from the pipe to prevent blocking the vt emulator.
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := r.vtEmu.Read(buf)
			if n > 0 {
				if r.promptActive.Load() {
					// Render mode: relay responses to agent
					r.mu.Lock()
					r.master.Write(buf[:n])
					r.mu.Unlock()
				}
				// Direct mode: discard (physical terminal responds)
			}
			if err != nil {
				return
			}
		}
	}()

	// Mark relay as ready for terminal prompts (stdin goroutine is about to start)
	r.promptReady.Store(true)

	// outer stdin → master (user keystrokes → agent)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				data := buf[:n]
				if r.promptActive.Load() {
					for _, b := range data {
						select {
						case r.promptCh <- b:
						default:
						}
					}
				} else {
					for len(data) > 0 {
						idx := bytes.IndexByte(data, 0x1a) // Ctrl-Z
						if idx == -1 {
							r.mu.Lock()
							r.master.Write(data)
							r.mu.Unlock()
							break
						}
						if idx > 0 {
							r.mu.Lock()
							r.master.Write(data[:idx])
							r.mu.Unlock()
						}
						r.suspend()
						data = data[idx+1:]
					}
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	// Wait for child to exit
	waitErr := r.cmd.Wait()
	signal.Stop(winchCh)
	signal.Stop(sigCh)

	// Signal shutdown — unblocks any active ShowPrompt/racePermission
	// so they can clean up before we tear down the terminal.
	close(r.shutdownCh)

	// Close master so the output copier finishes
	r.master.Close()
	r.master = nil

	// Drain remaining output
	<-done

	return waitErr
}

// renderLoop runs at ~60fps but only paints when a prompt is active.
// During direct mode (no prompt), it spins cheaply on atomic checks.
func (r *Relay) renderLoop() {
	ticker := time.NewTicker(16 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.renderDone:
			return
		case <-ticker.C:
			if r.promptActive.Load() && r.renderNeeded.CompareAndSwap(true, false) {
				r.render()
			}
		}
	}
}

// render paints the virtual terminal screen (and prompt) to stdout.
// Only called during render mode (prompt active).
// Because the outer terminal is in raw mode (OPOST disabled), we must use
// \r\n for line breaks.
func (r *Relay) render() {
	rendered := r.vtEmu.Render()

	var buf bytes.Buffer
	buf.Grow(len(rendered) + 256)
	buf.WriteString("\033[?25l") // hide cursor during render
	buf.WriteString("\033[H")    // cursor home

	// Write rendered agent screen, clearing trailing content on each line.
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		buf.WriteString(line)
		buf.WriteString("\033[K") // clear to end of line
		if i < len(lines)-1 {
			buf.WriteString("\r\n")
		}
	}

	// Draw prompt below the agent area
	if pd := r.promptContent.Load(); pd != nil {
		buf.WriteString("\r\n")
		buf.WriteString(pd.separator)
		buf.WriteString("\033[K\r\n")
		buf.WriteString(pd.toolLine)
		buf.WriteString("\033[K\r\n")
		buf.WriteString(pd.choices)
		buf.WriteString("\033[K\r\n")
		buf.WriteString(pd.input)
		buf.WriteString("\033[K")
	}

	// Position cursor at prompt input and show it
	if pd := r.promptContent.Load(); pd != nil {
		outerWs, err := getWinsize(os.Stdin.Fd())
		if err == nil {
			fmt.Fprintf(&buf, "\033[%d;%dH", outerWs.Row, len(pd.input)+1)
		}
		buf.WriteString("\033[?25h")
	} else {
		// No prompt content (cleanup render) — position at emulator cursor
		pos := r.vtEmu.CursorPosition()
		fmt.Fprintf(&buf, "\033[%d;%dH", pos.Y+1, pos.X+1)
		if r.vtCursorVisible.Load() {
			buf.WriteString("\033[?25h")
		}
	}

	os.Stdout.Write(buf.Bytes())
}

// suspend stops the relay and suspends the process for shell job control.
// When the user resumes (e.g. via "fg"), it re-enters raw mode and continues.
func (r *Relay) suspend() {
	r.restoreTermios()

	// Reset SIGTSTP to default so the kill actually stops us
	signal.Reset(syscall.SIGTSTP)
	syscall.Kill(0, syscall.SIGTSTP)
	// Execution resumes here after SIGCONT (e.g. "fg")

	if err := r.setRaw(); err != nil {
		log.Printf("warn: setRaw after resume: %v", err)
	}
	if err := r.syncWinsize(); err != nil {
		log.Printf("warn: syncWinsize after resume: %v", err)
	}
}

// Inject writes data directly to the PTY master as if it were typed.
// Safe to call from any goroutine.
func (r *Relay) Inject(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.master.Write(data)
	return err
}

func (r *Relay) cleanup() {
	// Clear any active prompt
	r.promptContent.Store(nil)
	r.promptActive.Store(false)

	// Stop the render loop
	select {
	case <-r.renderDone:
	default:
		close(r.renderDone)
	}

	// Close the emulator's pipe so the response reader goroutine exits
	if r.vtEmu != nil {
		r.vtEmu.Close()
	}

	r.restoreTermios()
	os.Stdout.WriteString("\033[?25h") // ensure cursor visible
	// Only reset terminal state if the child was killed — on normal exit
	// the child cleans up after itself and we don't want to clear its output.
	if r.killed {
		os.Stdout.WriteString("\033[?1049l") // leave alternate screen buffer
	}
	if r.master != nil {
		r.master.Close()
	}
	if r.slave != nil {
		r.slave.Close()
	}
}

// CloseWS shuts down the WebSocket client. Call after draining the bridge.
func (r *Relay) CloseWS() {
	if r.ws != nil {
		r.ws.Close()
	}
}

func (r *Relay) syncWinsize() error {
	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil {
		return err
	}
	return setWinsize(r.master.Fd(), ws)
}

func (r *Relay) setRaw() error {
	fd := int(os.Stdin.Fd())

	// Save current termios
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlReadTermios,
		uintptr(ptrOf(&r.origTermios)),
	); errno != 0 {
		return errno
	}

	raw := r.origTermios
	// cfmakeraw equivalent:
	// Input flags: disable break, CR-to-NL, parity, strip, flow control
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	// Output flags: disable post-processing
	raw.Oflag &^= syscall.OPOST
	// Control flags: character size 8, no parity
	raw.Cflag &^= syscall.PARENB | syscall.CSIZE
	raw.Cflag |= syscall.CS8
	// Local flags: disable echo, canonical, signals, extended
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON |
		syscall.ISIG | syscall.IEXTEN
	// Read returns after 1 byte, no timeout
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlWriteTermios,
		uintptr(ptrOf(&raw)),
	); errno != 0 {
		return errno
	}
	return nil
}

func (r *Relay) restoreTermios() {
	fd := int(os.Stdin.Fd())
	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlWriteTermios,
		uintptr(ptrOf(&r.origTermios)),
	)
}

// ShowPrompt switches from direct mode to render mode, draws a permission
// prompt below the agent area, and waits for the user to press a key.
// Agent output continues flowing to the vt emulator and is rendered in the
// top portion by the render loop.
// Returns 0 for allow, 1 for always_allow, 2 for deny, 3 for deny_stop,
// or an error if the context is cancelled (server responded first).
func (r *Relay) ShowPrompt(ctx context.Context, toolName, detail string) (int, error) {
	if !r.promptReady.Load() {
		return -1, fmt.Errorf("relay not ready for prompts")
	}

	r.promptMu.Lock()
	defer r.promptMu.Unlock()

	const promptHeight uint16 = 4

	ws, err := getWinsize(os.Stdin.Fd())
	if err != nil {
		return -1, err
	}
	if ws.Row <= promptHeight+5 {
		return -1, fmt.Errorf("terminal too small for prompt")
	}

	agentRows := ws.Row - promptHeight

	// Resize virtual terminal and PTY for the agent
	r.vtEmu.Resize(int(ws.Col), int(agentRows))
	agentWs := &Winsize{Row: agentRows, Col: ws.Col, Xpixel: ws.Xpixel, Ypixel: ws.Ypixel}
	setWinsize(r.master.Fd(), agentWs)

	// Drain stale keystrokes from previous prompts
	for {
		select {
		case <-r.promptCh:
		default:
			goto drained
		}
	}
drained:

	// Build prompt content for the render loop
	sep := fmt.Sprintf("\033[2m%s\033[0m", strings.Repeat("\u2500", int(ws.Col)))
	line := fmt.Sprintf(" \033[1m%s\033[0m: %s", toolName, detail)
	maxLen := int(ws.Col) - 1
	if len(toolName)+len(detail)+4 > maxLen {
		avail := maxLen - len(toolName) - 7
		if avail > 0 && avail < len(detail) {
			detail = detail[:avail] + "..."
		}
		line = fmt.Sprintf(" \033[1m%s\033[0m: %s", toolName, detail)
	}

	pd := &promptData{
		separator: sep,
		toolLine:  line,
		choices:   " [1] Allow  [2] Always allow  [3] Deny  [4] Deny & stop",
		input:     " Choice: ",
		height:    promptHeight,
	}

	// Switch to render mode: direct stdout stops, render loop takes over.
	r.promptContent.Store(pd)
	r.promptActive.Store(true)

	// Force an immediate first render so the prompt appears without
	// waiting for the next 16ms tick.
	r.renderNeeded.Store(true)

	defer func() {
		// Clear prompt content first (render() will see no prompt)
		r.promptContent.Store(nil)

		// Restore full size
		currentWs, wsErr := getWinsize(os.Stdin.Fd())
		if wsErr != nil {
			currentWs = ws
		}
		r.vtEmu.Resize(int(currentWs.Col), int(currentWs.Row))
		setWinsize(r.master.Fd(), currentWs)

		// Final render with full agent screen, no prompt — ensures the
		// physical terminal matches the vt emulator before switching back
		// to direct mode.
		r.render()

		// Switch back to direct mode
		r.promptActive.Store(false)
	}()

	// Wait for valid keystroke, context cancellation, or relay shutdown.
	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-r.shutdownCh:
			return -1, fmt.Errorf("relay shutting down")
		case b := <-r.promptCh:
			switch b {
			case '1':
				return 0, nil // allow
			case '2':
				return 1, nil // always_allow
			case '3':
				return 2, nil // deny
			case '4':
				return 3, nil // deny_stop
			}
		}
	}
}
