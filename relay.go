//go:build darwin || linux

package main

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Relay holds the state for a running PTY relay session.
type Relay struct {
	cmd    *exec.Cmd
	master *os.File
	slave  *os.File
	mu     sync.Mutex // serializes writes to master
	wsConn WSConn     // WebSocket interface
	killed bool       // true if the child was killed (not normal exit)

	// Shutdown coordination — closed when the child process exits.
	shutdownCh chan struct{}

	// Readiness signal — closed (once) on the first PTY output from the agent,
	// i.e. once it has started painting its TUI. Used to gate the autopilot
	// initial-prompt injection on a real "agent is up" condition instead of a
	// fixed timer. Only wired in daemon mode (RunDaemon).
	readyCh   chan struct{}
	readyOnce sync.Once

	// lastOutputAt is the unix-nanos timestamp of the most recent PTY output
	// chunk, updated on every read in RunDaemon. injectInitialPrompt polls it to
	// detect output quiescence — the TUI has finished its initial paint and is
	// waiting for input — which is far more reliable than a fixed settle timer
	// for deciding when the composer can accept a submit.
	lastOutputAt atomic.Int64

	// Terminal permission prompt support.
	promptReady atomic.Bool // true once stdin goroutine is running
	promptMu    sync.Mutex  // serializes prompts (one at a time)
	promptCh    chan byte   // keystrokes redirected here during prompt

	// Daemon mode: PTY output goes to daemonWriter instead of os.Stdout,
	// and terminal raw mode is managed by the client, not the relay.
	daemonMode   bool
	daemonWriter io.Writer
	daemonMu     sync.RWMutex

	// PTY screen tap (issue #38). Non-nil only for daemon-mode sessions whose
	// agent supports composer-suggestion extraction (see NewDaemon).
	// Observe-only; the composer suggestion is read on demand via
	// screen.suggestion.
	screen *screenTap
}

// Inject writes data directly to the PTY master as if it were typed.
// Safe to call from any goroutine.
func (r *Relay) Inject(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.master.Write(data)
	return err
}
