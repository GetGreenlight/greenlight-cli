//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"sync"
)

// Relay holds the state for a detached PTY — one per live agent instance.
// The PTY output is drained internally; user-visible activity flows via the
// agent's transcript JSONL + bridge streamer, not through this PTY.
type Relay struct {
	cmd    *exec.Cmd
	master *os.File
	slave  *os.File
	mu     sync.Mutex // serializes writes to master
	wsConn WSConn     // WebSocket interface
	killed bool       // true if the child was killed (not normal exit)

	// Shutdown coordination — closed when the child process exits.
	shutdownCh chan struct{}
}

// Inject writes data directly to the PTY master as if it were typed.
// Safe to call from any goroutine.
func (r *Relay) Inject(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.master.Write(data)
	return err
}
