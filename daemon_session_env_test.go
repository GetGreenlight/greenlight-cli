//go:build darwin || linux

package main

import (
	"strings"
	"testing"
)

func TestStripSSHIdentity(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"TERM=xterm",
		"SSH_AGENT_PID=1234",
		"SSH_AUTH_SOCK=/tmp/dup.sock", // duplicates must all be dropped — last wins at exec time
		"SSH_CONNECTION=host 22",      // other SSH_* vars are untouched
	}
	got := stripSSHIdentity(in)
	want := []string{"PATH=/usr/bin", "TERM=xterm", "SSH_CONNECTION=host 22"}
	if len(got) != len(want) {
		t.Fatalf("stripSSHIdentity = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stripSSHIdentity[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// envHas reports whether the child env slice carries the named variable.
func envHas(env []string, name string) bool {
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); ok && k == name {
			return true
		}
	}
	return false
}

// newDaemonEnv builds a Relay via NewDaemon and returns its child env,
// releasing the PTY it opened. Exercises the real child-env assembly, not a
// reimplementation of it.
func newDaemonEnv(t *testing.T, clientEnv map[string]string, sshIsolated bool) []string {
	t.Helper()
	r, err := NewDaemon("true", nil, map[string]string{}, t.TempDir(), nil, clientEnv, "", "codex", sshIsolated)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	r.master.Close()
	r.slave.Close()
	return r.cmd.Env
}

// TestNewDaemonSSHIsolationEnv covers both child-env base branches (#249):
// the clientEnv map from a connecting client, and the daemon's os.Environ()
// fallback for detached spawns. Isolation on strips SSH_AUTH_SOCK and
// SSH_AGENT_PID from either base; off leaves both present (the opt-in
// promise).
func TestNewDaemonSSHIsolationEnv(t *testing.T) {
	clientEnv := map[string]string{
		"PATH":           "/usr/bin",
		"SSH_AUTH_SOCK":  "/tmp/client-agent.sock",
		"SSH_AGENT_PID":  "4321",
		"SSH_CONNECTION": "host 22",
	}
	t.Setenv("SSH_AUTH_SOCK", "/tmp/daemon-agent.sock")
	t.Setenv("SSH_AGENT_PID", "1234")

	for _, c := range []struct {
		name      string
		clientEnv map[string]string
	}{
		{"clientEnv base", clientEnv},
		{"os.Environ fallback", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			off := newDaemonEnv(t, c.clientEnv, false)
			if !envHas(off, "SSH_AUTH_SOCK") || !envHas(off, "SSH_AGENT_PID") {
				t.Errorf("isolation off must pass the ssh-agent vars through; env=%v", off)
			}
			on := newDaemonEnv(t, c.clientEnv, true)
			if envHas(on, "SSH_AUTH_SOCK") {
				t.Error("isolation on must strip SSH_AUTH_SOCK")
			}
			if envHas(on, "SSH_AGENT_PID") {
				t.Error("isolation on must strip SSH_AGENT_PID")
			}
		})
	}

	// Other SSH_* vars survive the strip (only the two agent vars go).
	on := newDaemonEnv(t, clientEnv, true)
	if !envHas(on, "SSH_CONNECTION") {
		t.Error("isolation must not touch SSH_* vars other than the agent pair")
	}
}
