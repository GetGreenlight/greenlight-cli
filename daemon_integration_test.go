//go:build integration

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"greenlight/internal/mockserver"
)

// startTestDaemon starts a daemon process with a unique socket path so it
// doesn't conflict with other tests or a real daemon. Returns the socket path,
// a TMPDIR for the test, and a cleanup function.
//
// If a deviceID is provided (via the GREENLIGHT_DEVICE_ID env override in
// extraEnv), the test should pre-enroll a host with the test server and pass
// GREENLIGHT_DAEMON_SESSION_ID so the daemon's WebSocket can connect.
func startTestDaemon(t *testing.T, extraEnv ...string) (sockPath, tmpDir string, cleanup func()) {
	sockPath, tmpDir, _, cleanup = startTestDaemonWithHome(t, t.TempDir(), extraEnv...)
	return
}

// startTestDaemonWithHome is like startTestDaemon but lets the caller
// supply the daemon's HOME so it can pre-populate paths the daemon will
// scan (e.g. ~/.claude/projects/... for transcript tests).
func startTestDaemonWithHome(t *testing.T, home string, extraEnv ...string) (sockPath, tmpDir, daemonHome string, cleanup func()) {
	t.Helper()
	daemonHome = home
	tmpDir = t.TempDir()

	// Use a short, unique socket path (Unix sockets limited to ~104 bytes)
	sockPath = fmt.Sprintf("/tmp/gl-test-%d.sock", int64(os.Getpid())^time.Now().UnixNano())

	cmd := exec.Command(greenlightBin, "daemon", "start", "--foreground")
	// Put mock_claude's directory at the front of PATH so the daemon
	// resolves it (rather than a real claude on the dev machine) when it
	// looks up the agent binary for entitlement checks. The client's
	// PATH already prefers mock_claude, but the daemon does its own
	// resolution before the spawn.
	daemonPath := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")
	env := []string{
		"HOME=" + home,
		"PATH=" + daemonPath,
		"TMPDIR=" + tmpDir,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	daemonLogPath := filepath.Join(home, ".greenlight", "daemon.log")

	cleanup = func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			cmd.Wait()
		}
		os.Remove(sockPath)
		// Print daemon log for debugging failed tests
		if t.Failed() {
			t.Logf("daemon log:\n%s", readFileOrEmpty(daemonLogPath))
		}
	}

	if !waitForSocket(t, sockPath, 5*time.Second) {
		cleanup()
		t.Fatalf("daemon socket did not appear; stderr=%q; log=%q",
			stderr.String(), readFileOrEmpty(daemonLogPath))
	}

	return
}

// ---------- daemon start/stop/status ----------

func TestIntegration_Daemon_StartStop(t *testing.T) {
	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	// Check status via IPC
	resp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if resp.Type != "status_response" {
		t.Errorf("expected status_response, got %q", resp.Type)
	}
	if len(resp.Sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(resp.Sessions))
	}

	// Stop via IPC
	resp = daemonIPC(t, sockPath, ipcRequest{Type: "stop"})
	if resp.Type != "ok" {
		t.Errorf("expected ok, got %q", resp.Type)
	}
}

func TestIntegration_Daemon_StatusNotRunning(t *testing.T) {
	r := run(t, []string{"daemon", "status"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-nonexistent.sock",
	}, "")
	if !strings.Contains(r.Stderr, "not running") {
		t.Errorf("expected 'not running', got stderr=%q", r.Stderr)
	}
}

func TestIntegration_Daemon_StopNotRunning(t *testing.T) {
	r := run(t, []string{"daemon", "stop"}, []string{
		"HOME=" + t.TempDir(),
		"GREENLIGHT_DAEMON_SOCK=/tmp/gl-test-nonexistent.sock",
	}, "")
	if !strings.Contains(r.Stderr, "not running") {
		t.Errorf("expected 'not running', got stderr=%q", r.Stderr)
	}
}

// ---------- daemon connect flow ----------

func TestIntegration_Daemon_ConnectFlow(t *testing.T) {
	testServerURL.ClearHandlers()

	// Pre-enroll a host so the daemon's WebSocket can connect with a
	// known device_id / session_id pair.
	hostID := enrollTestHost(t, "test-dev")

	sockPath, tmpDir, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
		"GREENLIGHT_DISABLE_INTERPOSE=1",
	)
	defer cleanup()

	workDir := t.TempDir()
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	// Allocate a PTY for the client so raw mode works
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	// Run connect (which will talk to the daemon)
	client := exec.Command(greenlightBin, "connect",
		"--device-id", "test-dev",
		"--project", "test-proj",
	)
	client.Dir = workDir
	clientLog := filepath.Join(tmpDir, "client.log")
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
	}
	var clientStderr bytes.Buffer
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = &clientStderr
	defer func() {
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
	}()

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	// Wait for client to exit — mock claude exits quickly.
	// The client writes to its stderr (captured) and PTY (master).
	select {
	case err := <-done:
		t.Logf("client exited: err=%v stderr=%q", err, clientStderr.String())
	case <-time.After(10 * time.Second):
		client.Process.Kill()
		t.Fatalf("client timed out; stderr=%q", clientStderr.String())
	}

	// Read whatever output came through the PTY
	master.SetReadDeadline(time.Now().Add(1 * time.Second))
	var ptyBuf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := master.Read(tmp)
		if n > 0 {
			ptyBuf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	output := ptyBuf.String()
	t.Logf("PTY output: %q", output)

	// Verify host enrollment happened (test setup pre-enrolls so the daemon
	// WebSocket can connect). Per-session info travels over the daemon WS,
	// not via /session/enroll.
	enrollReqs := testServerURL.Requests("/session/enroll")
	if len(enrollReqs) == 0 {
		t.Fatal("expected enrollment request")
	}
	var enrollBody map[string]string
	if err := json.Unmarshal(enrollReqs[0].Body, &enrollBody); err != nil {
		t.Fatalf("parse enroll body: %v", err)
	}
	if enrollBody["device_id"] != "test-dev" {
		t.Errorf("expected device_id=test-dev, got %q", enrollBody["device_id"])
	}
}

// ---------- daemon connect with input injection ----------

func TestIntegration_Daemon_InputInjection(t *testing.T) {
	testServerURL.ClearHandlers()

	hostID := enrollTestHost(t, "test-dev")

	sockPath, tmpDir, cleanup := startTestDaemon(t,
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID="+hostID,
		"GREENLIGHT_DISABLE_INTERPOSE=1",
	)
	defer cleanup()

	workDir := t.TempDir()
	outputFile := filepath.Join(workDir, "claude-received.txt")
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	client := exec.Command(greenlightBin, "connect",
		"--device-id", "test-dev",
		"--project", "test-proj",
	)
	client.Dir = workDir
	clientLog := filepath.Join(tmpDir, "client.log")
	client.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"MOCK_CLAUDE_OUTPUT=" + outputFile,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
	}
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave
	defer func() {
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
	}()

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	go func() { done <- client.Wait() }()

	// Wait for mock claude to start
	got := readPTYUntil(t, master, "MOCK_CLAUDE_STARTED", 10*time.Second)
	if !strings.Contains(got, "MOCK_CLAUDE_STARTED") {
		// Client may have exited early — surface that and bail rather than
		// write to a closed PTY.
		select {
		case err := <-done:
			t.Fatalf("client exited before mock claude started: err=%v pty=%q", err, got)
		default:
			t.Fatalf("mock claude did not start within 10s, pty=%q", got)
		}
	}

	// Send input through the PTY master — this goes to the client,
	// which forwards it to the daemon, which writes to the agent PTY
	time.Sleep(500 * time.Millisecond)
	if _, err := master.Write([]byte("DAEMON_INPUT_TEST\n")); err != nil {
		t.Fatalf("write to PTY: %v", err)
	}

	// Wait for client to exit
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		client.Process.Kill()
		t.Fatal("client timed out")
	}

	// Verify mock claude received the input
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("mock claude output not created: %v", err)
	}
	if !strings.Contains(string(data), "DAEMON_INPUT_TEST") {
		t.Errorf("expected DAEMON_INPUT_TEST, got %q", string(data))
	}

}

// ---------- daemon connect error handling ----------

func TestIntegration_Daemon_ConnectError(t *testing.T) {
	testServerURL.ClearHandlers()

	sockPath, _, cleanup := startTestDaemon(t)
	defer cleanup()

	// Send a connect request with no device ID — should get an error response
	resp := daemonIPC(t, sockPath, ipcRequest{
		Type: "connect",
		Cwd:  t.TempDir(),
	})

	if resp.Type != "error" {
		t.Errorf("expected error response, got %q", resp.Type)
	}
	if resp.Message == "" {
		t.Error("expected non-empty error message")
	}

	// Verify daemon is still healthy after the error
	statusResp := daemonIPC(t, sockPath, ipcRequest{Type: "status"})
	if statusResp.Type != "status_response" {
		t.Errorf("daemon unhealthy after error: got %q", statusResp.Type)
	}
}

// ---------- helpers ----------

// waitForSocket waits for a Unix socket to become connectable.
func waitForSocket(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// daemonIPC sends a control message to the daemon and returns the response.
func daemonIPC(t *testing.T, sockPath string, req ipcRequest) ipcResponse {
	t.Helper()

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer conn.Close()

	data, _ := json.Marshal(req)
	data = append(data, '\n')
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write to daemon: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read from daemon: %v", err)
	}

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("parse daemon response: %v (raw: %s)", err, string(line))
	}
	return resp
}

// readFileOrEmpty reads a file and returns its contents, or empty string on error.
func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// TestIntegration_Daemon_ListSkills exercises the full list_skills control
// frame round-trip: server pushes list_skills over /ws/daemon, the daemon
// scans the session's cwd for installed skills, and replies with
// skills_listed. This is the canonical template for any test that needs
// to drive the daemon via a server-originated control frame.
func TestIntegration_Daemon_ListSkills(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// Pre-install a skill into the session's cwd so listSkills finds it.
	writeTestSkill(t, cs.Workdir, "demo-skill")

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}

	reply := awaitSkillsListed(t, cs.Sess, 5*time.Second)
	if reply.RelayID != cs.Sess.RelayID {
		t.Errorf("relay_id mismatch: got %q want %q", reply.RelayID, cs.Sess.RelayID)
	}
	if len(reply.Skills) != 1 || reply.Skills[0] != "demo-skill" {
		t.Errorf("skills mismatch: got %v want [demo-skill]", reply.Skills)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_ListSkills_Empty verifies the daemon replies
// with an empty array (not null) when no skills are installed. The
// distinction matters on the wire — clients distinguish "session has no
// skills" from "session unknown" by reply presence.
func TestIntegration_Daemon_ListSkills_Empty(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}

	reply := awaitSkillsListed(t, cs.Sess, 5*time.Second)
	if reply.Skills == nil {
		t.Fatal("expected empty slice, got nil — wire form must be [] not null")
	}
	if len(reply.Skills) != 0 {
		t.Errorf("expected no skills, got %v", reply.Skills)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_SkillInstall verifies the daemon writes skills
// delivered via the session_started ack to disk and that listSkills
// finds them afterwards. Round-trip: SetSessionStartHook returns a skill
// in the ack → daemon installs → server queries via list_skills →
// reply contains the installed skill name.
func TestIntegration_Daemon_SkillInstall(t *testing.T) {
	testServerURL.ClearHandlers()
	testServerURL.SetSessionStartHook(func(relayID string, _ json.RawMessage) any {
		return map[string]any{
			"type":     "session_started",
			"relay_id": relayID,
			"skills": []map[string]any{
				{
					"name":     "delivered-skill",
					"skill_md": "---\nname: delivered-skill\ndescription: server-delivered\n---\nbody\n",
				},
			},
		}
	})

	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// The skill should be on disk under the agent's skills root.
	skillMD := filepath.Join(cs.Workdir, ".claude", "skills", "_greenlight", "delivered-skill", "SKILL.md")
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("expected delivered skill on disk at %s: %v", skillMD, err)
	}

	// And listSkills should see it.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}
	reply := awaitSkillsListed(t, cs.Sess, 5*time.Second)
	if len(reply.Skills) != 1 || reply.Skills[0] != "delivered-skill" {
		t.Errorf("skills mismatch: got %v want [delivered-skill]", reply.Skills)
	}

	cs.Wait(10 * time.Second)

	// And cleaned up after the session ends (no other sessions share the
	// workdir, so the cleanup guard allows removal).
	if _, err := os.Stat(skillMD); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed after session end, got err=%v", skillMD, err)
	}
}

// TestIntegration_Daemon_Kill verifies the server can terminate a
// running session by sending a kill control frame as a binary
// WebSocket message. The frame routes through the daemon's controlFunc
// → routeControlFrame → per-session killFunc, terminating the agent
// process group.
func TestIntegration_Daemon_Kill(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "kill",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send kill: %v", err)
	}

	if err := cs.WaitDone(10 * time.Second); err != nil {
		t.Fatalf("client did not exit after kill: %v", err)
	}
}

// TestIntegration_Daemon_Permission_Allow exercises the full
// interpose→daemon→server permission round-trip with an "allow"
// decision. mock_claude exec()s a non-safe-list binary, the C library
// intercepts the spawn, the request flows over /ws/daemon as
// permission_request, the mock server replies allow keyed by
// request_id, the spawn proceeds, and mock_claude reports "ok".
//
// macOS-only: on Linux, the Go runtime's direct syscall use evades
// LD_PRELOAD for many operations, and the spawn-helper-based gating
// path differs in ways the existing seccomp/agent_test.py harness
// already covers.
func TestIntegration_Daemon_Permission_Allow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("interpose spawn interception via DYLD is macOS-specific in this harness")
	}
	runPermissionTest(t, "allow")
}

// TestIntegration_Daemon_Permission_Deny is the soft-deny counterpart.
// The mock server denies the spawn; the C library writes
// "[GREENLIGHT] Permission denied." to stderr, _exits 126, and
// mock_claude reports "err: exit status 126" plus the captured stderr.
func TestIntegration_Daemon_Permission_Deny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("interpose spawn interception via DYLD is macOS-specific in this harness")
	}
	runPermissionTest(t, "deny")
}

// TestIntegration_Daemon_Permission_Stop is the deny+stop counterpart.
// The mock server denies with interrupt:true; the C library writes
// "[GREENLIGHT] Operation not permitted. Stop all work immediately."
// to stderr and _exits 137. Exercises the stop branch the agent system
// prompt teaches the model to treat as a hard halt.
func TestIntegration_Daemon_Permission_Stop(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("interpose spawn interception via DYLD is macOS-specific in this harness")
	}
	runPermissionTest(t, "deny_stop")
}

// runPermissionTest is the shared body for the allow/deny variants.
func runPermissionTest(t *testing.T, decision string) {
	t.Helper()
	testServerURL.ClearHandlers()

	workDir := t.TempDir()
	resultPath := filepath.Join(workDir, "exec-result")
	interposeLog := filepath.Join(t.TempDir(), "interpose.log")
	cs, cleanup := startConnectSession(t, connectOpts{
		EnableInterpose:      true,
		SkipMockClaudeOutput: true,
		AgentEnv: []string{
			// /bin/sleep is not in the C library's safe-spawn list, so it
			// triggers a permission request. 0.01s is fast enough not to
			// stretch the test if the spawn is allowed.
			"MOCK_CLAUDE_EXEC=/bin/sleep 0.01",
			"MOCK_CLAUDE_EXEC_RESULT=" + resultPath,
			"GREENLIGHT_INTERPOSE_LOG=" + interposeLog,
		},
	})
	defer cleanup()
	defer func() {
		if t.Failed() {
			t.Logf("interpose log:\n%s", readFileOrEmpty(interposeLog))
		}
	}()

	stop := startPermissionAutoResponder(t, cs.Sess, func(pr permissionRequest) string {
		return decision
	})

	// Drain the PTY in the background to keep the master from blocking
	// on a full buffer; we don't assert on its contents because the
	// agent's stderr (where the [GREENLIGHT] denial banner is written)
	// may not be relayed onto the connect-side PTY in daemon mode.
	go drainPTY(cs.Master, 15*time.Second)

	if err := cs.WaitDone(15 * time.Second); err != nil {
		t.Fatalf("client did not exit: %v", err)
	}
	seen := stop()

	if !sawSpawnOf("sleep", seen) {
		t.Errorf("expected a Bash permission_request invoking sleep, got %v",
			summarizeRequests(seen))
	}

	result := readFileOrEmpty(resultPath)
	switch decision {
	case "allow":
		if !strings.HasPrefix(result, "ok") {
			t.Errorf("expected exec ok, got %q", result)
		}
	case "deny":
		if !strings.Contains(result, "exit status 126") {
			t.Errorf("expected exit status 126 (soft deny), got %q", result)
		}
		if !strings.Contains(result, "[GREENLIGHT] Permission denied.") {
			t.Errorf("expected soft-deny stderr banner, got %q", result)
		}
	case "deny_stop":
		if !strings.Contains(result, "exit status 137") {
			t.Errorf("expected exit status 137 (stop), got %q", result)
		}
		if !strings.Contains(result, "[GREENLIGHT] Operation not permitted. Stop all work immediately.") {
			t.Errorf("expected stop stderr banner, got %q", result)
		}
	}
}

// TestIntegration_Daemon_Seccomp_Allow exercises the Linux seccomp
// supervisor permission round-trip with an "allow" decision. mock_claude
// (built with CGO so the interpose .so loads via LD_PRELOAD) opens a
// file for writing under the user's real $HOME — outside the
// supervisor's auto-allow zones (tmp, system, dotfile, agent-internal).
// The kernel triggers SECCOMP_RET_USER_NOTIF; the supervisor reads the
// path from /proc/<pid>/mem, classifies it as a Write tool call, and
// long-polls the daemon WS. The mock server auto-allows; the supervisor
// continues the syscall; mock_claude reports "ok".
//
// Linux-only: the seccomp USER_NOTIF mechanism requires kernel 5.9+.
// macOS uses the dyld interpose path tested by Permission_Allow/Deny.
func TestIntegration_Daemon_Seccomp_Allow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("seccomp supervisor is Linux-only")
	}
	runSeccompTest(t, "allow")
}

// TestIntegration_Daemon_Seccomp_Deny is the soft-deny counterpart.
// The mock server denies; the supervisor returns EACCES for the openat;
// mock_claude reports an error containing "permission denied".
func TestIntegration_Daemon_Seccomp_Deny(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("seccomp supervisor is Linux-only")
	}
	runSeccompTest(t, "deny")
}

// TestIntegration_Daemon_Seccomp_Stop is the deny+stop counterpart.
// The mock server denies with interrupt:true; the supervisor returns
// EPERM for the openat; mock_claude reports an error containing
// "operation not permitted". Exercises the stop branch the agent
// system prompt teaches the model to treat as a hard halt.
func TestIntegration_Daemon_Seccomp_Stop(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("seccomp supervisor is Linux-only")
	}
	runSeccompTest(t, "deny_stop")
}

// runSeccompTest is the shared body for the allow/deny variants.
func runSeccompTest(t *testing.T, decision string) {
	t.Helper()
	testServerURL.ClearHandlers()

	// The write target must live outside /tmp (which the seccomp
	// supervisor auto-allows). Create a fresh dir under the real user
	// HOME — the test's HOME override is itself under /tmp, which won't
	// trip the filter.
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		t.Skip("cannot determine real user HOME for seccomp write target")
	}
	writeDir, err := os.MkdirTemp(realHome, "greenlight-test-seccomp-*")
	if err != nil {
		t.Fatalf("mkdir under HOME: %v", err)
	}
	defer os.RemoveAll(writeDir)
	writeFile := filepath.Join(writeDir, "out.txt")
	// Result file lives under /tmp so it bypasses the seccomp filter —
	// otherwise a "deny" decision would also block the result write,
	// leaving us unable to observe the outcome.
	resultFile := filepath.Join(t.TempDir(), "result.txt")

	cs, cleanup := startConnectSession(t, connectOpts{
		EnableInterpose:      true,
		SkipMockClaudeOutput: true,
		AgentEnv: []string{
			"MOCK_CLAUDE_WRITE_FILE=" + writeFile,
			"MOCK_CLAUDE_WRITE_RESULT=" + resultFile,
		},
	})
	defer cleanup()

	stop := startPermissionAutoResponder(t, cs.Sess, func(pr permissionRequest) string {
		return decision
	})

	// Drain the PTY in the background so the buffer doesn't fill.
	go drainPTY(cs.Master, 15*time.Second)

	if err := cs.WaitDone(15 * time.Second); err != nil {
		t.Fatalf("client did not exit: %v", err)
	}
	seen := stop()

	// Look for a Write request targeting our file.
	if !sawWriteOf(writeFile, seen) {
		t.Errorf("expected a Write permission_request for %s, got %v",
			writeFile, summarizeRequests(seen))
	}

	result := readFileOrEmpty(resultFile)
	switch decision {
	case "allow":
		if !strings.HasPrefix(result, "ok") {
			t.Errorf("expected write ok, got %q", result)
		}
		if _, err := os.Stat(writeFile); err != nil {
			t.Errorf("expected target file written: %v", err)
		}
	case "deny":
		if !strings.Contains(strings.ToLower(result), "permission denied") {
			t.Errorf("expected 'permission denied' (EACCES) in result, got %q", result)
		}
	case "deny_stop":
		if !strings.Contains(strings.ToLower(result), "operation not permitted") {
			t.Errorf("expected 'operation not permitted' (EPERM) in result, got %q", result)
		}
	}
}

// sawWriteOf returns true if any observed permission request was a
// Write/Edit on the given file path.
func sawWriteOf(path string, reqs []permissionRequest) bool {
	for _, r := range reqs {
		if r.Data.Tool != "Write" && r.Data.Tool != "Edit" {
			continue
		}
		if fp, _ := r.Data.Input["file_path"].(string); fp == path {
			return true
		}
	}
	return false
}

// sawSpawnOf returns true if any observed permission request was a Bash
// invocation whose command contains the given binary name.
func sawSpawnOf(name string, reqs []permissionRequest) bool {
	for _, r := range reqs {
		if r.Data.Tool != "Bash" {
			continue
		}
		if cmd, _ := r.Data.Input["command"].(string); strings.Contains(cmd, name) {
			return true
		}
	}
	return false
}

func summarizeRequests(reqs []permissionRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = fmt.Sprintf("%s(%v)", r.Data.Tool, r.Data.Input)
	}
	return out
}

// drainPTY reads from a PTY master for up to timeout, accumulating
// whatever bytes arrive. Returns when the read times out (no more data
// for one polling interval) or the timeout elapses overall.
func drainPTY(master *os.File, timeout time.Duration) string {
	var buf bytes.Buffer
	deadline := time.Now().Add(timeout)
	tmp := make([]byte, 4096)
	for time.Now().Before(deadline) {
		master.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, err := master.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if os.IsTimeout(err) {
				if buf.Len() > 0 {
					return buf.String()
				}
				continue
			}
			break
		}
	}
	return buf.String()
}

// TestIntegration_Daemon_SessionTranscript exercises the
// session_transcript control frame end-to-end:
//
//   - Plant a Claude-format JSONL at the path the daemon's transcript
//     scanner would find (~/.claude/projects/<dir>/<sessionID>.jsonl
//     under the daemon's HOME, with a matching `cwd` field).
//   - Wait for startTranscriptStreamer to discover it and persist the
//     conversation→relay mapping.
//   - Send session_transcript over /ws/daemon, assert the reply carries
//     the entries we wrote, in order.
//
// Per-agent transcript-format normalization (codex/copilot/cursor/pi
// transformers) is exercised separately in stream_test.go.
func TestIntegration_Daemon_SessionTranscript(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// Plant a Claude transcript at ~/.claude/projects/<anything>/<convID>.jsonl
	// in the daemon's HOME. The basename (minus ext) becomes the
	// conversation ID once startTranscriptStreamer notices the file.
	projDir := filepath.Join(cs.DaemonHome, ".claude", "projects", "test-proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	convID := "abcd1234-5678-90ab-cdef-1234567890ab"
	transcriptPath := filepath.Join(projDir, convID+".jsonl")
	lines := []string{
		`{"type":"summary","summary":"Test session","cwd":"` + cs.Workdir + `"}`,
		`{"type":"user","message":{"role":"user","content":"hello"},"cwd":"` + cs.Workdir + `"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hi"},"cwd":"` + cs.Workdir + `"}`,
	}
	if err := os.WriteFile(transcriptPath,
		[]byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Directly seed the conversation_id → relay_id mapping the daemon's
	// handler reads via lookupConversationID. The transcript streamer
	// would normally populate this once it discovered the agent's real
	// transcript, but mock_claude doesn't write one, so we write it
	// ourselves to get the same effect.
	seedRelayMapping(t, cs.DaemonHome, convID, cs.Sess.RelayID)

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "session_transcript",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send session_transcript: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "session_transcript_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("did not receive session_transcript_response within 5s")
	}

	var reply struct {
		Type    string            `json:"type"`
		RelayID string            `json:"relay_id"`
		Agent   string            `json:"agent"`
		Message string            `json:"message"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse session_transcript_response: %v", err)
	}
	if reply.Message != "" {
		t.Fatalf("unexpected error message: %q", reply.Message)
	}
	if reply.Agent != "claude-code" {
		t.Errorf("agent mismatch: got %q want claude-code", reply.Agent)
	}
	if len(reply.Entries) != 3 {
		t.Errorf("entries count: got %d want 3 (raw=%s)", len(reply.Entries), string(matched))
	}
	if len(reply.Entries) >= 2 {
		var entry struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(reply.Entries[1], &entry); err != nil || entry.Type != "user" {
			t.Errorf("entry[1] parse: type=%q err=%v", entry.Type, err)
		}
	}

	cs.Wait(10 * time.Second)
}

// seedRelayMapping writes a conversation_id → relay_id entry into the
// daemon's sessions.json (the file that lookupConversationID reads).
// Used by transcript tests to skip the live transcript-discovery path
// and get straight to the handler under test.
func seedRelayMapping(t *testing.T, daemonHome, convID, relayID string) {
	t.Helper()
	dir := filepath.Join(daemonHome, ".greenlight")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions.json")
	m := map[string]string{convID: relayID}
	if existing, err := os.ReadFile(path); err == nil {
		json.Unmarshal(existing, &m)
		m[convID] = relayID
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_Daemon_SessionTranscript_NoFile verifies the
// graceful-error path when the daemon can't find a transcript for the
// requested relay_id. Reply should carry a non-empty message and zero
// entries (rather than crashing or omitting the response).
func TestIntegration_Daemon_SessionTranscript_NoFile(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	// No transcript file planted — daemon's scan should return empty.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "session_transcript",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send session_transcript: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "session_transcript_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("expected session_transcript_response (with error) within 5s")
	}

	var reply struct {
		Message string            `json:"message"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reply.Message == "" {
		t.Error("expected error message, got empty")
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_HistoryEntry_Roundtrip pushes a history_entry
// control frame to record a permission outcome, then queries via
// project_history and asserts the entry comes back.
func TestIntegration_Daemon_HistoryEntry_Roundtrip(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	entry := map[string]any{
		"request_id":   "req-1",
		"tool_name":    "Bash",
		"tool_input":   map[string]any{"command": "ls"},
		"outcome":      "allowed",
		"agent":        "claude",
		"responded_at": "2026-05-09T00:00:00Z",
	}
	if err := cs.Sess.SendBinary(map[string]any{
		"type":    "history_entry",
		"project": "test-proj",
		"entry":   entry,
	}); err != nil {
		t.Fatalf("send history_entry: %v", err)
	}

	// Give the daemon a tick to flush the entry to disk.
	time.Sleep(150 * time.Millisecond)

	if err := cs.Sess.SendBinary(map[string]any{
		"type":    "project_history",
		"project": "test-proj",
	}); err != nil {
		t.Fatalf("send project_history: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "project_history_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("did not receive project_history_response within 5s")
	}

	var reply struct {
		Type    string `json:"type"`
		Project string `json:"project"`
		Entries []struct {
			RequestID string `json:"request_id"`
			ToolName  string `json:"tool_name"`
			Outcome   string `json:"outcome"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse project_history_response: %v (raw=%s)", err, string(matched))
	}
	if reply.Project != "test-proj" {
		t.Errorf("project mismatch: got %q", reply.Project)
	}
	found := false
	for _, e := range reply.Entries {
		if e.RequestID == "req-1" && e.ToolName == "Bash" && e.Outcome == "allowed" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("did not find req-1 in entries: %+v", reply.Entries)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_SessionHistory_ControlFrame validates the
// control-frame variant of session_history (distinct from the IPC-based
// daemon status query). The reply lists persisted sessionRecord
// entries; we don't assert on contents (the live test session may or
// may not produce a record by the time we ask), only on the wire shape.
func TestIntegration_Daemon_SessionHistory_ControlFrame(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type": "session_history",
	}); err != nil {
		t.Fatalf("send session_history: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "session_history_response"
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("did not receive session_history_response within 5s")
	}

	var reply struct {
		Type    string            `json:"type"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse session_history_response: %v (raw=%s)", err, string(matched))
	}
	if reply.Entries == nil {
		// Empty slice is fine; nil suggests the daemon serialized null
		// where it should have used [].
		t.Errorf("entries was nil — should be [] on the wire")
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_DeleteSession_Unknown verifies the daemon
// gracefully handles delete_session for a relay_id with no persisted
// record (the common case when the session never produced a transcript
// — saveSessionRecord is gated on transcript availability). The daemon
// should log and ignore, not crash or reply.
func TestIntegration_Daemon_DeleteSession_Unknown(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "delete_session",
		"relay_id": "00000000-0000-0000-0000-deadbeef0000",
	}); err != nil {
		t.Fatalf("send delete_session: %v", err)
	}

	// Give the daemon a moment to handle the frame, then confirm it's
	// still alive by hitting it with a list_skills round-trip we already
	// know works.
	time.Sleep(150 * time.Millisecond)
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills: %v", err)
	}
	awaitSkillsListed(t, cs.Sess, 5*time.Second)

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_NewSession_Validation drives the new_session
// control frame through its error paths (missing cwd, non-directory cwd,
// unknown agent) and verifies the daemon replies with
// new_session_result {success:false, error:…} for each rather than crashing
// or hanging. The happy path is not exercised here — it spawns a real
// terminal window which we can't drive in CI.
func TestIntegration_Daemon_NewSession_Validation(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	cases := []struct {
		name      string
		payload   map[string]any
		wantErrIn string
	}{
		{
			name: "missing_cwd",
			payload: map[string]any{
				"type":       "new_session",
				"request_id": "missing-cwd",
				"agent":      "claude",
			},
			wantErrIn: "cwd",
		},
		{
			name: "cwd_not_directory",
			payload: map[string]any{
				"type":       "new_session",
				"request_id": "bad-cwd",
				"cwd":        "/definitely/does/not/exist/greenlight-test",
				"agent":      "claude",
			},
			wantErrIn: "not a directory",
		},
		{
			name: "unknown_agent",
			payload: map[string]any{
				"type":       "new_session",
				"request_id": "bad-agent",
				"cwd":        cs.Workdir,
				"agent":      "not-a-real-agent",
			},
			wantErrIn: "unknown agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqID, _ := tc.payload["request_id"].(string)
			if err := cs.Sess.SendBinary(tc.payload); err != nil {
				t.Fatalf("send new_session: %v", err)
			}
			matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
				var m struct {
					Type      string `json:"type"`
					RequestID string `json:"request_id"`
				}
				if err := json.Unmarshal(raw, &m); err != nil {
					return false
				}
				return m.Type == "new_session_result" && m.RequestID == reqID
			}, 5*time.Second)
			if matched == nil {
				t.Fatalf("no new_session_result for request_id=%q", reqID)
			}
			var reply struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(matched, &reply); err != nil {
				t.Fatalf("unmarshal reply: %v", err)
			}
			if reply.Success {
				t.Fatalf("expected success=false, got reply=%s", string(matched))
			}
			if !strings.Contains(reply.Error, tc.wantErrIn) {
				t.Errorf("error %q does not contain %q", reply.Error, tc.wantErrIn)
			}
		})
	}

	// Confirm the daemon is still healthy after rejecting bad input.
	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_skills",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_skills after new_session errors: %v", err)
	}
	awaitSkillsListed(t, cs.Sess, 5*time.Second)

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_ListTickets_HappyPath drives the full
// list_tickets round-trip end to end against a fake GitHub server: the
// daemon resolves the repo from the session's cwd via `git remote
// get-url origin`, fetches the encrypted GITHUB_ACCESS_TOKEN through
// the mock server's WS, decrypts it locally, hits the fake GitHub API,
// filters out the PR, and replies with a tickets_listed frame
// containing only the real issue.
func TestIntegration_Daemon_ListTickets_HappyPath(t *testing.T) {
	testServerURL.ClearHandlers()

	// 1. Pre-init an X25519 keypair under the daemon's HOME so
	// loadPrivateKey() succeeds when the daemon decrypts the token.
	// Encrypt the test token with the same pubkey so the mock server
	// can hand back a ciphertext the daemon can actually open.
	daemonHome := t.TempDir()
	priv, err := generateKeypair()
	if err != nil {
		t.Fatalf("generateKeypair: %v", err)
	}
	keyDir := filepath.Join(daemonHome, ".greenlight")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "key"),
		[]byte(base64.StdEncoding.EncodeToString(priv.Bytes())), 0600); err != nil {
		t.Fatal(err)
	}
	const fakeToken = "ghp_fake_token_for_test"
	tokenCT, err := encryptSecret(priv.PublicKey(), []byte(fakeToken))
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	tokenCTB64 := base64.StdEncoding.EncodeToString(tokenCT)

	// 2. Workdir is a real git repo with an origin pointing at github.com.
	// The daemon will parse owner/repo from this URL; the actual HTTP call
	// goes to the fake server, not github.com (overridden via env).
	workDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "protocol.file.allow=always", "remote", "add", "origin", "https://github.com/foo/bar.git"},
	} {
		c := exec.Command("git", args...)
		c.Dir = workDir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// 3. Fake GitHub API. Returns one real issue and one PR (the PR
	// must be filtered out by the daemon).
	var ghHits int
	var ghPath string
	var ghAuth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghHits++
		ghPath = r.URL.Path + "?" + r.URL.RawQuery
		ghAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
		  {"number":1,"title":"real issue","state":"open",
		   "html_url":"https://github.com/foo/bar/issues/1",
		   "updated_at":"2026-05-01T00:00:00Z",
		   "labels":[{"name":"bug"}]},
		  {"number":2,"title":"a PR","state":"open",
		   "html_url":"https://github.com/foo/bar/pull/2",
		   "updated_at":"2026-05-02T00:00:00Z",
		   "labels":[],
		   "pull_request":{"url":"x"}}
		]`)
	}))
	defer gh.Close()

	// 4. Mock-server hook: serve secrets_get for GITHUB_ACCESS_TOKEN.
	testServerURL.SetFrameHook(func(s *mockserver.Session, frame json.RawMessage) {
		var msg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(frame, &msg) != nil || msg.Type != "secrets_get" {
			return
		}
		var data map[string]interface{}
		json.Unmarshal(msg.Data, &data)
		reqID, _ := data["request_id"].(string)
		key, _ := data["key"].(string)
		if key != "GITHUB_ACCESS_TOKEN" {
			s.Send(map[string]any{
				"type": "secrets_get_response", "request_id": reqID,
				"error": "not_found",
			})
			return
		}
		s.Send(map[string]any{
			"type":       "secrets_get_response",
			"request_id": reqID,
			"ciphertext": tokenCTB64,
		})
	})

	// 5. Start the session with the pre-prepared workdir + daemon home +
	// the GitHub base URL pointed at our fake server.
	cs, cleanup := startConnectSession(t, connectOpts{
		WorkDir:    workDir,
		DaemonHome: daemonHome,
		DaemonEnv:  []string{"GREENLIGHT_GITHUB_API_BASE=" + gh.URL},
	})
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_tickets",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_tickets: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type    string `json:"type"`
			RelayID string `json:"relay_id"`
		}
		return json.Unmarshal(raw, &m) == nil &&
			m.Type == "tickets_listed" && m.RelayID == cs.Sess.RelayID
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("no tickets_listed reply")
	}

	var reply struct {
		Owner   string `json:"owner"`
		Repo    string `json:"repo"`
		Error   string `json:"error"`
		Tickets []struct {
			Number    int      `json:"number"`
			Title     string   `json:"title"`
			State     string   `json:"state"`
			Labels    []string `json:"labels"`
			HTMLURL   string   `json:"html_url"`
			UpdatedAt string   `json:"updated_at"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v (raw=%s)", err, string(matched))
	}
	if reply.Error != "" {
		t.Fatalf("unexpected error in reply: %q", reply.Error)
	}
	if reply.Owner != "foo" || reply.Repo != "bar" {
		t.Errorf("owner/repo = %q/%q, want foo/bar", reply.Owner, reply.Repo)
	}
	if len(reply.Tickets) != 1 {
		t.Fatalf("got %d tickets, want 1 (PR should be filtered); reply=%s",
			len(reply.Tickets), string(matched))
	}
	tk := reply.Tickets[0]
	if tk.Number != 1 || tk.Title != "real issue" || tk.State != "open" {
		t.Errorf("ticket mismatch: %+v", tk)
	}
	if len(tk.Labels) != 1 || tk.Labels[0] != "bug" {
		t.Errorf("labels = %v, want [bug]", tk.Labels)
	}

	// Verify the daemon actually hit the fake server with the decrypted
	// token in the Authorization header — the proxy path end-to-end.
	if ghHits != 1 {
		t.Errorf("github hits = %d, want 1", ghHits)
	}
	if !strings.Contains(ghPath, "/repos/foo/bar/issues") {
		t.Errorf("github path = %q, want /repos/foo/bar/issues...", ghPath)
	}
	if ghAuth != "Bearer "+fakeToken {
		t.Errorf("authorization header = %q, want %q", ghAuth, "Bearer "+fakeToken)
	}

	cs.Wait(10 * time.Second)
}

// TestIntegration_Daemon_ListTickets_NotARepo exercises the list_tickets
// error path: the session's cwd is a fresh tmpdir (not a git repo), so
// `git remote get-url origin` fails and the daemon must reply with
// tickets_listed carrying an error rather than hanging or crashing.
// Happy-path coverage would need a fake GitHub server and a stubbed
// secrets_get; defer until the iOS side exercises it end-to-end.
func TestIntegration_Daemon_ListTickets_NotARepo(t *testing.T) {
	testServerURL.ClearHandlers()
	cs, cleanup := startConnectSession(t)
	defer cleanup()

	if err := cs.Sess.SendBinary(map[string]any{
		"type":     "list_tickets",
		"relay_id": cs.Sess.RelayID,
	}); err != nil {
		t.Fatalf("send list_tickets: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type    string `json:"type"`
			RelayID string `json:"relay_id"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return false
		}
		return m.Type == "tickets_listed" && m.RelayID == cs.Sess.RelayID
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("no tickets_listed reply")
	}

	var reply struct {
		Tickets []map[string]any `json:"tickets"`
		Error   string           `json:"error"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Error == "" {
		t.Errorf("expected error for non-repo cwd, got reply=%s", string(matched))
	}
	if reply.Tickets == nil {
		t.Errorf("tickets must be [] on the wire, got nil; reply=%s", string(matched))
	}

	cs.Wait(10 * time.Second)
}

// ---------- skill helpers ----------

func writeTestSkill(t *testing.T, workdir, name string) {
	t.Helper()
	dir := filepath.Join(workdir, ".claude", "skills", "_greenlight", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: test\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

type skillsListedReply struct {
	Type    string   `json:"type"`
	RelayID string   `json:"relay_id"`
	Skills  []string `json:"skills"`
}

// permissionRequest is the payload of a permission_request frame as
// observed by the mock server. Only the fields the tests care about are
// modeled; any others are ignored.
type permissionRequest struct {
	Type string `json:"type"`
	Data struct {
		RequestID string                 `json:"request_id"`
		Tool      string                 `json:"tool_name"`
		Input     map[string]interface{} `json:"tool_input"`
	} `json:"data"`
}

// startPermissionAutoResponder runs a goroutine that watches the
// session's inbox for permission_request frames and sends a
// permission_response back. The decide callback returns "allow",
// "deny", or "deny_stop" given the request — "deny_stop" sets
// interrupt:true so the interpose library returns its stop variant
// (exit 137, "Operation not permitted" stderr). Calling the returned
// stop func waits for the goroutine and returns the requests it
// answered (in order).
func startPermissionAutoResponder(
	t *testing.T,
	sess *mockserver.Session,
	decide func(permissionRequest) string,
) func() []permissionRequest {
	t.Helper()
	stopCh := make(chan struct{})
	doneCh := make(chan []permissionRequest, 1)

	go func() {
		var seen []permissionRequest
		handled := make(map[string]bool)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			for _, raw := range sess.Inbox() {
				var hdr struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &hdr) != nil || hdr.Type != "permission_request" {
					continue
				}
				var pr permissionRequest
				if json.Unmarshal(raw, &pr) != nil || pr.Data.RequestID == "" || handled[pr.Data.RequestID] {
					continue
				}
				handled[pr.Data.RequestID] = true
				seen = append(seen, pr)
				behavior := decide(pr)
				_ = sess.Send(map[string]interface{}{
					"type":       "permission_response",
					"request_id": pr.Data.RequestID,
					"behavior":   behavior,
					"interrupt":  behavior == "deny_stop",
					"message":    "test " + behavior,
				})
			}
			select {
			case <-stopCh:
				doneCh <- seen
				return
			case <-ticker.C:
			}
		}
	}()

	return func() []permissionRequest {
		close(stopCh)
		return <-doneCh
	}
}

func awaitSkillsListed(t *testing.T, sess *mockserver.Session, timeout time.Duration) skillsListedReply {
	t.Helper()
	matched := sess.WaitForFrame(func(raw json.RawMessage) bool {
		var hdr struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(raw, &hdr) == nil && hdr.Type == "skills_listed"
	}, timeout)
	if matched == nil {
		t.Fatalf("did not receive skills_listed within %v", timeout)
	}
	var reply skillsListedReply
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("parse skills_listed: %v (raw=%s)", err, string(matched))
	}
	return reply
}

// connectSession is the live state needed to drive a `greenlight connect`
// session in a test: the running client process, the PTY master used to
// inject input, the agent workdir, the daemon's home dir (for tests
// that need to pre-populate scanned paths like ~/.claude/projects), and
// the registered mock-server session.
type connectSession struct {
	Client     *exec.Cmd
	Master     *os.File
	Workdir    string
	DaemonHome string
	Sess       *mockserver.Session
	done       chan error
}

// Wait sends "done\n" to mock_claude (which exits on stdin input) and
// waits for the client to exit, with a fallback kill on timeout.
func (cs *connectSession) Wait(timeout time.Duration) {
	cs.Master.Write([]byte("done\n"))
	select {
	case <-cs.done:
	case <-time.After(timeout):
		cs.Client.Process.Kill()
		<-cs.done
	}
}

// WaitDone blocks until the client exits (used after externally-driven
// shutdown like a kill control frame). Returns nil if the client exited
// (regardless of exit code — kill makes nonzero codes expected) and an
// error only on timeout.
func (cs *connectSession) WaitDone(timeout time.Duration) error {
	select {
	case <-cs.done:
		return nil
	case <-time.After(timeout):
		cs.Client.Process.Kill()
		<-cs.done
		return fmt.Errorf("timeout after %v", timeout)
	}
}

// connectOpts tunes startConnectSession. Zero value is the default
// (interpose disabled, mock_claude waits on stdin).
type connectOpts struct {
	// EnableInterpose runs the agent with the real interpose library
	// loaded. macOS only — Linux Go binaries make raw syscalls that
	// LD_PRELOAD can't catch.
	EnableInterpose bool
	// AgentEnv is appended to the connect client's env. Use this to set
	// MOCK_CLAUDE_* variables.
	AgentEnv []string
	// SkipMockClaudeOutput omits MOCK_CLAUDE_OUTPUT, letting mock_claude
	// run to completion instead of blocking on stdin.
	SkipMockClaudeOutput bool
	// WorkDir, if non-empty, is used as the connect client's working
	// directory instead of a fresh t.TempDir. Useful for tests that need
	// to pre-init the workdir (git repo, etc.).
	WorkDir string
	// DaemonHome, if non-empty, is used as the daemon's HOME instead of a
	// fresh t.TempDir. Useful for tests that need to pre-populate
	// ~/.greenlight (e.g. an X25519 keypair the daemon will use to decrypt
	// secrets fetched from the mock server).
	DaemonHome string
	// DaemonEnv is appended to the daemon's env. Use this for test-only
	// overrides like GREENLIGHT_GITHUB_API_BASE.
	DaemonEnv []string
}

// startConnectSession boots a daemon (with a fresh enrolled host),
// launches `greenlight connect` against mock_claude, and waits for the
// agent to start and the mock-server session to register. Returns the
// live state plus the test cleanup func to defer.
//
// The caller is responsible for resetting any mock-server hooks BEFORE
// invoking this — handlers set after this returns won't apply to the
// session_started ack.
func startConnectSession(t *testing.T, opts ...connectOpts) (*connectSession, func()) {
	t.Helper()
	var opt connectOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	hostID := enrollTestHost(t, "test-dev")
	daemonEnv := []string{
		"GREENLIGHT_DEVICE_ID=test-dev",
		"GREENLIGHT_DAEMON_SESSION_ID=" + hostID,
	}
	if !opt.EnableInterpose {
		daemonEnv = append(daemonEnv, "GREENLIGHT_DISABLE_INTERPOSE=1")
	}
	daemonEnv = append(daemonEnv, opt.DaemonEnv...)
	dHome := opt.DaemonHome
	if dHome == "" {
		dHome = t.TempDir()
	}
	sockPath, tmpDir, daemonHome, daemonCleanup := startTestDaemonWithHome(t, dHome, daemonEnv...)

	workDir := opt.WorkDir
	if workDir == "" {
		workDir = t.TempDir()
	}
	pathWithMock := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")

	master, slave, err := openPTY()
	if err != nil {
		daemonCleanup()
		t.Fatalf("openPTY: %v", err)
	}
	setWinsize(slave.Fd(), &Winsize{Row: 24, Col: 80})

	clientLog := filepath.Join(tmpDir, "client.log")
	client := exec.Command(greenlightBin, "connect",
		"--device-id", "test-dev",
		"--project", "test-proj",
	)
	client.Dir = workDir
	clientEnv := []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathWithMock,
		"TMPDIR=" + tmpDir,
		"TERM=xterm-256color",
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_LOG=" + clientLog,
	}
	if !opt.SkipMockClaudeOutput {
		clientEnv = append(clientEnv, "MOCK_CLAUDE_OUTPUT="+filepath.Join(workDir, "claude-stdin.txt"))
	}
	clientEnv = append(clientEnv, opt.AgentEnv...)
	client.Env = clientEnv
	client.Stdin = slave
	client.Stdout = slave
	client.Stderr = slave

	done := make(chan error, 1)
	if err := client.Start(); err != nil {
		master.Close()
		slave.Close()
		daemonCleanup()
		t.Fatalf("start client: %v", err)
	}
	slave.Close()
	// Close done after Wait so multiple receivers (test + cleanup) don't
	// deadlock when one of them has already drained the value.
	go func() {
		err := client.Wait()
		done <- err
		close(done)
	}()

	cleanup := func() {
		// If the client is still running, prod it. <-done is safe even
		// when already drained: the channel is closed and yields zero.
		if client.ProcessState == nil {
			client.Process.Kill()
		}
		<-done
		master.Close()
		if t.Failed() {
			t.Logf("client log:\n%s", readFileOrEmpty(clientLog))
		}
		daemonCleanup()
	}

	got := readPTYUntil(t, master, "MOCK_CLAUDE_STARTED", 10*time.Second)
	if !strings.Contains(got, "MOCK_CLAUDE_STARTED") {
		cleanup()
		t.Fatalf("mock claude did not start; pty=%q", got)
	}

	sess := waitForOneSession(t, testServerURL, 5*time.Second)

	return &connectSession{
		Client:     client,
		Master:     master,
		Workdir:    workDir,
		DaemonHome: daemonHome,
		Sess:       sess,
		done:       done,
	}, cleanup
}

// waitForOneSession blocks until the mock server has exactly one
// registered session and returns it. Fails the test on timeout.
func waitForOneSession(t *testing.T, srv *mockserver.Server, timeout time.Duration) *mockserver.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sessions := srv.Sessions()
		if len(sessions) == 1 {
			return sessions[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected 1 session within %v, got %d", timeout, len(srv.Sessions()))
	return nil
}

// readPTYUntil reads from a PTY master until the output contains the target
// string or the timeout expires.
func readPTYUntil(t *testing.T, master *os.File, target string, timeout time.Duration) string {
	t.Helper()

	var buf bytes.Buffer
	deadline := time.Now().Add(timeout)
	tmp := make([]byte, 4096)

	for time.Now().Before(deadline) {
		master.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := master.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if strings.Contains(buf.String(), target) {
				return buf.String()
			}
		}
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	return buf.String()
}
