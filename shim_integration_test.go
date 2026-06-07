//go:build integration

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestIntegration_Daemon_ShimActivation verifies the daemon installs a command
// shim (a symlink back to the greenlight binary) for a known tool exactly when
// its secret is stored — and not otherwise, so a user without the secret keeps
// their own credentials. Cross-platform: this exercises the daemon-side
// activation wiring (secrets_list probe → activeShims → installCommandShims),
// which doesn't depend on interpose.
func TestIntegration_Daemon_ShimActivation(t *testing.T) {
	t.Run("installs shim when the secret is present", func(t *testing.T) {
		testServerURL.ClearHandlers()
		testServerURL.SetSecrets("GITHUB_ACCESS_TOKEN")
		defer testServerURL.SetSecrets()

		// Tickets/shim secret must be configured — there's no built-in fallback.
		cs, cleanup := startConnectSession(t, connectOpts{
			ConfigSeed: map[string]string{"tickets_secret": "GITHUB_ACCESS_TOKEN"},
		})
		defer cleanup()

		link := filepath.Join(cs.DaemonTmp, "greenlight-bin-"+cs.Sess.RelayID, "gh")
		if !waitForSymlink(link, 5*time.Second) {
			t.Fatalf("expected gh shim symlink at %s", link)
		}
		// It must point at the greenlight binary (so argv[0]==gh dispatches
		// to runShim), and the baseline greenlight shim must coexist.
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", link, err)
		}
		wantTarget, _ := filepath.EvalSymlinks(greenlightBin)
		if target != wantTarget {
			t.Errorf("gh shim points at %s, want %s", target, wantTarget)
		}
		glShim := filepath.Join(cs.DaemonTmp, "greenlight-bin-"+cs.Sess.RelayID, "greenlight")
		if _, err := os.Lstat(glShim); err != nil {
			t.Errorf("baseline greenlight shim missing: %v", err)
		}

		cs.Wait(10 * time.Second)
	})

	t.Run("no shim when the configured secret isn't stored", func(t *testing.T) {
		testServerURL.ClearHandlers()
		testServerURL.SetSecrets() // none stored

		// Even with a secret configured, no shim activates unless it's stored —
		// there is no built-in token fallback.
		cs, cleanup := startConnectSession(t, connectOpts{
			ConfigSeed: map[string]string{"tickets_secret": "GITHUB_ACCESS_TOKEN"},
		})
		defer cleanup()

		// The baseline greenlight shim is always created; gh must not be.
		shimDir := filepath.Join(cs.DaemonTmp, "greenlight-bin-"+cs.Sess.RelayID)
		if !waitForSymlink(filepath.Join(shimDir, "greenlight"), 5*time.Second) {
			t.Fatalf("baseline greenlight shim never appeared in %s", shimDir)
		}
		if _, err := os.Lstat(filepath.Join(shimDir, "gh")); err == nil {
			t.Errorf("gh shim should not exist when the configured secret isn't stored")
		}

		cs.Wait(10 * time.Second)
	})
}

// TestIntegration_Daemon_ShimRewrite is the end-to-end proof on macOS: with a
// GitHub token secret stored, when the agent runs a bare `gh` command, the
// interpose layer intercepts the spawn and the permission request the phone
// receives is rewritten to the secret-injecting `greenlight run … -- gh …`
// form. This ties together activation, the PATH shim, and the display rewrite.
//
// It also exercises the loop guard: after approving the rewritten command, the
// shim execs the real `gh`. In this unsigned harness the interpose lib loads
// into the greenlight shim too, so that inner exec is intercepted — but the
// guard auto-allows it (it matches the just-approved command), so the phone
// sees exactly one prompt, not two.
func TestIntegration_Daemon_ShimRewrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("interpose spawn interception via DYLD is macOS-specific in this harness")
	}
	testServerURL.ClearHandlers()
	testServerURL.SetSecrets("GITHUB_ACCESS_TOKEN")
	defer testServerURL.SetSecrets()

	cs, cleanup := startConnectSession(t, connectOpts{
		EnableInterpose:      true,
		SkipMockClaudeOutput: true,
		ConfigSeed:           map[string]string{"tickets_secret": "GITHUB_ACCESS_TOKEN"},
		AgentEnv: []string{
			// Bare `gh` resolves to the per-session shim on PATH; --version
			// is instant and needs no repo/network for the post-allow exec.
			"MOCK_CLAUDE_EXEC=gh --version",
		},
	})
	defer cleanup()

	stop := startPermissionAutoResponder(t, cs.Sess, func(pr permissionRequest) string {
		return "allow"
	})
	go drainPTY(cs.Master, 15*time.Second)

	if err := cs.WaitDone(15 * time.Second); err != nil {
		t.Fatalf("client did not exit: %v", err)
	}
	seen := stop()

	// Exactly one prompt: the rewritten shim command. The shim's re-exec of
	// the real binary is auto-allowed by the loop guard, so it never reaches
	// the phone as a second request.
	const want = "greenlight run -e GH_TOKEN=GITHUB_ACCESS_TOKEN -- gh --version"
	var bashReqs []string
	for _, r := range seen {
		if r.Data.Tool == "Bash" {
			cmd, _ := r.Data.Input["command"].(string)
			bashReqs = append(bashReqs, cmd)
		}
	}
	if len(bashReqs) != 1 || bashReqs[0] != want {
		t.Errorf("expected exactly one rewritten Bash request %q, got %v", want, bashReqs)
	}
}

// waitForSymlink polls until path exists (as a symlink or file) or the
// timeout elapses.
func waitForSymlink(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
