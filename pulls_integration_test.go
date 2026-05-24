//go:build integration

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"greenlight/internal/mockserver"
)

// prSetup encapsulates the boilerplate shared by the open_pr / merge_pr
// integration tests: daemon home with an X25519 keypair, a mock server hook
// that hands back the encrypted GITHUB_ACCESS_TOKEN, and a fake GitHub server.
// Returns the values startConnectSession needs.
type prSetup struct {
	daemonHome string
	ghURL      string
	ghHits     *int
}

func setupPRTest(t *testing.T, ghHandler http.Handler) prSetup {
	t.Helper()
	testServerURL.ClearHandlers()

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

	var ghHits int
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghHits++
		ghHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(gh.Close)

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

	return prSetup{daemonHome: daemonHome, ghURL: gh.URL, ghHits: &ghHits}
}

// initTicketRepo creates a git repo configured as if it had been cloned
// from github.com/foo/bar. A local bare repo stands in for the remote;
// `insteadOf` rewrites https://github.com/foo/bar.git → bare so pushes
// transparently resolve while parseGitHubRemote still sees a github URL.
// The repo is checked out to `gl/<n>-x` (no commits past origin/main
// unless the caller adds them). Returns the workdir and the bare path.
func initTicketRepo(t *testing.T, n int) (string, string) {
	t.Helper()
	bare := t.TempDir()
	mustGit(t, bare, "init", "-q", "--bare", "-b", "main")

	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "initial")
	mustGit(t, dir, "remote", "add", "origin", "https://github.com/foo/bar.git")
	// pushInsteadOf rewrites the URL for pushes only; `git remote get-url
	// origin` (used by repoFromCwd) still returns the github.com URL so
	// parseGitHubRemote can extract owner/repo. fetches use the bare path
	// for refs/remotes/origin/* via `git fetch <bare>`.
	mustGit(t, dir, "config", "--local",
		"url."+bare+".pushInsteadOf", "https://github.com/foo/bar.git")
	// Push initial main to the bare repo. Since fetch goes through the
	// github URL (not rewritten), we push by explicit URL and then build
	// the tracking ref manually.
	mustGit(t, dir, "push", "-q", bare, "main")
	mustGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	mustGit(t, dir, "checkout", "-q", "-b", fmt.Sprintf("gl/%d-x", n))
	return dir, bare
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
}

// ---------- merge_pr ----------

func TestIntegration_Daemon_MergePR_HappyPath(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	setup := setupPRTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"sha":"deadbeef","merged":true,"message":"ok"}`)
	}))

	cs, cleanup := startConnectSession(t, connectOpts{
		DaemonHome: setup.daemonHome,
		DaemonEnv:  []string{"GREENLIGHT_GITHUB_API_BASE=" + setup.ghURL},
	})
	defer cleanup()

	reqID := "merge-req-1"
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "merge_pr",
		"relay_id":   "",
		"request_id": reqID,
		"owner":      "foo",
		"repo":       "bar",
		"number":     451,
		"method":     "squash",
	}); err != nil {
		t.Fatalf("send merge_pr: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &m) == nil &&
			m.Type == "merge_pr_result" && m.RequestID == reqID
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("no merge_pr_result reply")
	}

	var reply struct {
		Success bool   `json:"success"`
		SHA     string `json:"sha"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reply.Success || reply.SHA != "deadbeef" || reply.Error != "" {
		t.Errorf("reply = %+v", reply)
	}
	if gotMethod != "PUT" || gotPath != "/repos/foo/bar/pulls/451/merge" {
		t.Errorf("github call = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer ghp_fake_token_for_test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if *setup.ghHits != 1 {
		t.Errorf("github hits = %d, want 1", *setup.ghHits)
	}
	cs.Wait(10 * time.Second)
}

// 405 from GitHub (non-mergeable) must surface in the .error field so iOS
// can render the message.
func TestIntegration_Daemon_MergePR_NotMergeable(t *testing.T) {
	setup := setupPRTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"message":"Branch is not mergeable"}`)
	}))

	cs, cleanup := startConnectSession(t, connectOpts{
		DaemonHome: setup.daemonHome,
		DaemonEnv:  []string{"GREENLIGHT_GITHUB_API_BASE=" + setup.ghURL},
	})
	defer cleanup()

	reqID := "merge-req-2"
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "merge_pr",
		"relay_id":   "",
		"request_id": reqID,
		"owner":      "foo",
		"repo":       "bar",
		"number":     7,
	}); err != nil {
		t.Fatalf("send merge_pr: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &m) == nil &&
			m.Type == "merge_pr_result" && m.RequestID == reqID
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("no merge_pr_result reply")
	}
	var reply struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Success {
		t.Errorf("expected success=false")
	}
	if !strings.Contains(reply.Error, "Branch is not mergeable") {
		t.Errorf("error %q should surface GitHub message", reply.Error)
	}
	cs.Wait(10 * time.Second)
}

// ---------- open_pr ----------

// open_pr must refuse when the workdir has no commits ahead of origin/<base>.
// We point cwd at a fresh repo on `gl/42-x` with no commits past base.
func TestIntegration_Daemon_OpenPR_NoCommits(t *testing.T) {
	setup := setupPRTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("github should not be hit when no_commits refuses early")
		w.WriteHeader(http.StatusInternalServerError)
	}))

	workDir, _ := initTicketRepo(t, 42)

	cs, cleanup := startConnectSession(t, connectOpts{
		WorkDir:    workDir,
		DaemonHome: setup.daemonHome,
		DaemonEnv:  []string{"GREENLIGHT_GITHUB_API_BASE=" + setup.ghURL},
	})
	defer cleanup()

	reqID := "open-req-no-commits"
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "open_pr",
		"relay_id":   cs.Sess.RelayID,
		"request_id": reqID,
		"title":      "T",
		"body":       "B",
	}); err != nil {
		t.Fatalf("send open_pr: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &m) == nil &&
			m.Type == "open_pr_result" && m.RequestID == reqID
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("no open_pr_result reply")
	}
	var reply struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Success {
		t.Errorf("expected success=false")
	}
	if reply.Error != "no_commits" {
		t.Errorf("error = %q, want no_commits", reply.Error)
	}
	cs.Wait(10 * time.Second)
}

// open_pr must refuse a dirty working tree.
func TestIntegration_Daemon_OpenPR_DirtyWorktree(t *testing.T) {
	setup := setupPRTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("github should not be hit when dirty_worktree refuses early")
	}))

	workDir, _ := initTicketRepo(t, 43)
	// Add a commit so commitCount is non-zero, then dirty the tree so the
	// dirty check trips instead of no_commits.
	mustGit(t, workDir, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "extra")
	if err := os.WriteFile(filepath.Join(workDir, "untracked"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	cs, cleanup := startConnectSession(t, connectOpts{
		WorkDir:    workDir,
		DaemonHome: setup.daemonHome,
		DaemonEnv:  []string{"GREENLIGHT_GITHUB_API_BASE=" + setup.ghURL},
	})
	defer cleanup()

	reqID := "open-req-dirty"
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "open_pr",
		"relay_id":   cs.Sess.RelayID,
		"request_id": reqID,
		"title":      "T",
		"body":       "B",
	}); err != nil {
		t.Fatalf("send open_pr: %v", err)
	}
	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &m) == nil &&
			m.Type == "open_pr_result" && m.RequestID == reqID
	}, 5*time.Second)
	if matched == nil {
		t.Fatal("no open_pr_result reply")
	}
	var reply struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Success || reply.Error != "dirty_worktree" {
		t.Errorf("reply = %+v, want success=false error=dirty_worktree", reply)
	}
	cs.Wait(10 * time.Second)
}

// open_pr happy path: set up a local bare repo as origin, put a commit on
// `gl/42-x` past `origin/main`, and let the daemon push + open the PR
// against the fake GitHub.
func TestIntegration_Daemon_OpenPR_HappyPath(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]string
	setup := setupPRTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number": 911, "html_url": "https://github.com/foo/bar/pull/911"}`)
	}))

	workDir, bare := initTicketRepo(t, 42)
	// Add a real commit on gl/42-x so commitCount > 0 and there's something
	// to push.
	mustGit(t, workDir, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "the work")

	cs, cleanup := startConnectSession(t, connectOpts{
		WorkDir:    workDir,
		DaemonHome: setup.daemonHome,
		DaemonEnv:  []string{"GREENLIGHT_GITHUB_API_BASE=" + setup.ghURL},
	})
	defer cleanup()

	reqID := "open-req-happy"
	if err := cs.Sess.SendBinary(map[string]any{
		"type":       "open_pr",
		"relay_id":   cs.Sess.RelayID,
		"request_id": reqID,
		"title":      "PR title",
		"body":       "PR body\n\nFixes #42",
	}); err != nil {
		t.Fatalf("send open_pr: %v", err)
	}

	matched := cs.Sess.WaitForFrame(func(raw json.RawMessage) bool {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		return json.Unmarshal(raw, &m) == nil &&
			m.Type == "open_pr_result" && m.RequestID == reqID
	}, 10*time.Second)
	if matched == nil {
		t.Fatal("no open_pr_result reply")
	}
	var reply struct {
		Success  bool   `json:"success"`
		PRNumber int    `json:"pr_number"`
		PRURL    string `json:"pr_url"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(matched, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reply.Success {
		t.Fatalf("reply error: %q", reply.Error)
	}
	if reply.PRNumber != 911 || reply.PRURL != "https://github.com/foo/bar/pull/911" {
		t.Errorf("reply = %+v", reply)
	}
	if gotMethod != "POST" || gotPath != "/repos/foo/bar/pulls" {
		t.Errorf("github call = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer ghp_fake_token_for_test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody["head"] != "gl/42-x" || gotBody["base"] != "main" {
		t.Errorf("head/base = %q/%q", gotBody["head"], gotBody["base"])
	}
	// Confirm the branch was actually pushed to the bare remote.
	out, err := exec.Command("git", "-C", bare, "rev-parse", "gl/42-x").CombinedOutput()
	if err != nil {
		t.Errorf("branch not pushed to bare: %v: %s", err, out)
	}
	cs.Wait(15 * time.Second)
}
