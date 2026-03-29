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

	// Terminal permission prompt support.
	promptReady atomic.Bool // true once stdin goroutine is running
	promptMu    sync.Mutex  // serializes prompts (one at a time)
	promptCh    chan byte    // keystrokes redirected here during prompt

	// Daemon mode: PTY output goes to daemonWriter instead of os.Stdout,
	// and terminal raw mode is managed by the client, not the relay.
	daemonMode   bool
	daemonWriter io.Writer
	daemonMu     sync.RWMutex
}

// Inject writes data directly to the PTY master as if it were typed.
// Safe to call from any goroutine.
func (r *Relay) Inject(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.master.Write(data)
	return err
}
