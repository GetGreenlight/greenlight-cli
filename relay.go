//go:build darwin || linux

package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Relay holds the state for a running PTY relay session.
type Relay struct {
	cmd         *exec.Cmd
	master      *os.File
	slave       *os.File
	origTermios syscall.Termios
	mu          sync.Mutex // serializes writes to master
	ws          *WSClient  // optional WebSocket client
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

	// Strip GREENLIGHT_* vars inherited from the parent, then add
	// the explicit exportEnvs (which may include GREENLIGHT_* keys).
	env := os.Environ()
	cmd.Env = make([]string, 0, len(env)+len(exportEnvs))
	for _, e := range env {
		if !strings.HasPrefix(e, "GREENLIGHT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	for k, v := range exportEnvs {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	r := &Relay{
		cmd:    cmd,
		master: master,
		slave:  slave,
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

	// Handle SIGWINCH — forward window resize to inner PTY
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			if err := r.syncWinsize(); err != nil {
				log.Printf("warn: syncWinsize on SIGWINCH: %v", err)
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

	// master → outer stdout (child output → user's terminal)
	// If WebSocket is connected, also send output to the remote server.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.master.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
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

	// outer stdin → master (user keystrokes → Claude Code)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				data := buf[:n]
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

	// Close master so the output copier finishes
	r.master.Close()
	r.master = nil

	// Drain remaining output
	<-done

	return waitErr
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
	r.restoreTermios()
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
