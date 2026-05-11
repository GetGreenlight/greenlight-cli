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
	"sync"
	"testing"
	"time"

	"greenlight/internal/mockserver"
)

// TestIntegration_OAuth_RefreshSuffixedAccessToken exercises the full OAuth
// refresh path end-to-end against the mock relay server, asserting that an
// access-token key with a user-added suffix (e.g. GOOGLE_ACCESS_TOKEN_WORK)
// causes the refresh lookup to use the matching GOOGLE_REFRESH_TOKEN_WORK key
// — not GOOGLE_REFRESH_TOKEN. Regression test for b26e081.
func TestIntegration_OAuth_RefreshSuffixedAccessToken(t *testing.T) {
	testServerURL.ClearHandlers()

	const (
		accessKey     = "GOOGLE_ACCESS_TOKEN_WORK"
		refreshKey    = "GOOGLE_REFRESH_TOKEN_WORK"
		refreshPlain  = "rt-original-value"
		newAccessTok  = "at-fresh-from-google"
	)

	// Pre-init keypair under the home that `greenlight run` will use, so
	// loadPrivateKey() succeeds in the child process. Encrypting the
	// refresh ciphertext here uses the same keypair the child will load.
	home := t.TempDir()
	priv, err := generateKeypair()
	if err != nil {
		t.Fatalf("generateKeypair: %v", err)
	}
	keyDir := filepath.Join(home, ".greenlight")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "key"),
		[]byte(base64.StdEncoding.EncodeToString(priv.Bytes())), 0600); err != nil {
		t.Fatal(err)
	}
	refreshCT, err := encryptSecret(priv.PublicKey(), []byte(refreshPlain))
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	refreshCTB64 := base64.StdEncoding.EncodeToString(refreshCT)

	// Mock OAuth provider token endpoint. Records the form-encoded body so
	// the test can assert the refresh_token it received.
	var oauthMu sync.Mutex
	var oauthHits int
	var oauthBody string
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		oauthMu.Lock()
		oauthHits++
		oauthBody = string(body)
		oauthMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, newAccessTok)
	}))
	defer oauthSrv.Close()

	// Frame hook on the relay: serve the WS round-trips that oauth.go and
	// run.go drive through the daemon. Track the keys looked up so the
	// test can assert the suffix was preserved.
	var hookMu sync.Mutex
	var getKeys, putKeys []string
	testServerURL.SetFrameHook(func(s *mockserver.Session, frame json.RawMessage) {
		var msg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(frame, &msg) != nil {
			return
		}
		var data map[string]interface{}
		json.Unmarshal(msg.Data, &data)
		reqID, _ := data["request_id"].(string)
		key, _ := data["key"].(string)

		switch msg.Type {
		case "secrets_get":
			hookMu.Lock()
			getKeys = append(getKeys, key)
			hookMu.Unlock()
			switch key {
			case accessKey:
				// First lookup: stored access token has expired.
				s.Send(map[string]any{
					"type":       "secrets_get_response",
					"request_id": reqID,
					"error":      "expired",
				})
			case refreshKey:
				s.Send(map[string]any{
					"type":       "secrets_get_response",
					"request_id": reqID,
					"ciphertext": refreshCTB64,
				})
			default:
				s.Send(map[string]any{
					"type":       "secrets_get_response",
					"request_id": reqID,
					"error":      "not_found",
				})
			}
		case "secrets_put":
			hookMu.Lock()
			putKeys = append(putKeys, key)
			hookMu.Unlock()
			s.Send(map[string]any{
				"type":       "secrets_put_response",
				"request_id": reqID,
			})
		case "list_oauth_providers":
			s.Send(map[string]any{
				"type":       "oauth_providers_response",
				"request_id": reqID,
				"providers": []map[string]any{{
					"id":            "google",
					"token_url":     oauthSrv.URL,
					"client_id":     "test-client",
					"client_secret": "test-secret",
				}},
			})
		}
	})

	hostID := enrollTestHost(t, "oauth-suffix-dev")
	sockPath, _, cleanup := startTestDaemonWithHomeAndSock(t, home, hostID)
	defer cleanup()

	// Invoke `greenlight run -e GOOGLE_ACCESS_TOKEN_WORK -- /bin/sh -c 'exit 0'`.
	// Success means: secrets_get returned "expired", refresh path triggered,
	// looked up GOOGLE_REFRESH_TOKEN_WORK (suffix preserved), exchanged at
	// the OAuth endpoint, and stored the new value back under the same
	// suffixed access key.
	r := run(t, []string{"run", "-e", accessKey, "--", "/bin/sh", "-c", "exit 0"}, []string{
		"HOME=" + home,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
	}, "")
	if r.ExitCode != 0 {
		t.Fatalf("greenlight run exited %d; stderr=%q", r.ExitCode, r.Stderr)
	}

	hookMu.Lock()
	defer hookMu.Unlock()

	// Both access keys must be the suffixed form, never the bare prefix.
	wantGet := []string{accessKey, refreshKey}
	if len(getKeys) != 2 || getKeys[0] != wantGet[0] || getKeys[1] != wantGet[1] {
		t.Errorf("secrets_get keys = %v, want %v", getKeys, wantGet)
	}
	for _, k := range getKeys {
		if k == "GOOGLE_REFRESH_TOKEN" {
			t.Errorf("refresh lookup used bare prefix %q — suffix was stripped (regression)", k)
		}
	}

	if len(putKeys) != 1 || putKeys[0] != accessKey {
		t.Errorf("secrets_put keys = %v, want [%q]", putKeys, accessKey)
	}

	oauthMu.Lock()
	defer oauthMu.Unlock()
	if oauthHits != 1 {
		t.Errorf("oauth token endpoint hits = %d, want 1", oauthHits)
	}
	if !strings.Contains(oauthBody, "refresh_token="+refreshPlain) {
		t.Errorf("oauth POST body missing refresh_token=%s; got %q", refreshPlain, oauthBody)
	}
	if !strings.Contains(oauthBody, "grant_type=refresh_token") {
		t.Errorf("oauth POST body missing grant_type; got %q", oauthBody)
	}
}

// startTestDaemonWithHomeAndSock starts a daemon bound to the given HOME and
// the given pre-enrolled session ID, returning the IPC socket path. Mirrors
// startTestDaemon but lets the caller share HOME between the test process
// (which seeds ~/.greenlight/key) and the daemon/CLI children.
func startTestDaemonWithHomeAndSock(t *testing.T, home, hostID string) (string, string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	sockPath := fmt.Sprintf("/tmp/gl-test-%d.sock", int64(os.Getpid())^time.Now().UnixNano())

	daemonPath := filepath.Dir(mockClaudeBin) + ":" + os.Getenv("PATH")
	cmd := exec.Command(greenlightBin, "daemon", "start", "--foreground")
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + daemonPath,
		"TMPDIR=" + tmpDir,
		"GREENLIGHT_DAEMON_SOCK=" + sockPath,
		"GREENLIGHT_DEVICE_ID=oauth-suffix-dev",
		"GREENLIGHT_DAEMON_SESSION_ID=" + hostID,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	cleanup := func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			cmd.Wait()
		}
		os.Remove(sockPath)
		if t.Failed() {
			t.Logf("daemon log:\n%s", readFileOrEmpty(filepath.Join(home, ".greenlight", "daemon.log")))
		}
	}

	if !waitForSocket(t, sockPath, 5*time.Second) {
		cleanup()
		t.Fatalf("daemon socket did not appear")
	}
	return sockPath, tmpDir, cleanup
}
