//go:build darwin || linux

package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// TestSSHKeygenRoundTrip is the keygen acceptance criterion: generate →
// encrypt (as keygen stores it) → decrypt → parse → sign, and the signature
// verifies against the public key line that would land in the .pub file.
func TestSSHKeygenRoundTrip(t *testing.T) {
	pemBytes, pubLine, err := generateSSHKeyMaterial("greenlight:test@host")
	if err != nil {
		t.Fatalf("generateSSHKeyMaterial: %v", err)
	}
	if !strings.HasPrefix(pubLine, "ssh-ed25519 ") {
		t.Fatalf("pub line should be an ed25519 authorized_keys entry: %q", pubLine)
	}
	if !strings.HasSuffix(pubLine, " greenlight:test@host") {
		t.Errorf("pub line missing comment: %q", pubLine)
	}
	// The plaintext PEM must never appear in the stored form.
	glPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptSecret(glPriv.PublicKey(), pemBytes)
	if err != nil {
		t.Fatalf("encryptSecret: %v", err)
	}
	if bytes.Contains(ct, pemBytes) {
		t.Fatal("ciphertext contains plaintext private key")
	}

	plain, err := decryptSecret(glPriv, ct)
	if err != nil {
		t.Fatalf("decryptSecret: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(plain)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	data := []byte("challenge")
	sig, err := signer.Sign(rand.Reader, data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v", err)
	}
	if err := pub.Verify(data, sig); err != nil {
		t.Errorf("signature does not verify against the .pub: %v", err)
	}
}

func TestAuthorizedKeysLineRestrict(t *testing.T) {
	line := "ssh-ed25519 AAAA test"
	if got := authorizedKeysLine(line, false); got != "restrict "+line {
		t.Errorf("default must be restrict-hardened, got %q", got)
	}
	if got := authorizedKeysLine(line, true); got != line {
		t.Errorf("--unrestricted must omit the prefix, got %q", got)
	}
}

func TestSSHNameMapping(t *testing.T) {
	if got := sshSecretName("staging"); got != "SSH_KEY_STAGING" {
		t.Errorf("sshSecretName = %q", got)
	}
	if got := sshShortName("SSH_KEY_STAGING"); got != "staging" {
		t.Errorf("sshShortName = %q", got)
	}
	// A hand-written entry without the prefix degrades to its lowercase form.
	if got := sshShortName("MyKey"); got != "mykey" {
		t.Errorf("sshShortName no-prefix = %q", got)
	}
}

// newTestAgentKey generates a key and returns the resolved sshSessionKey and
// its plaintext PEM (standing in for the decrypted secret).
func newTestAgentKey(t *testing.T, name string) (sshSessionKey, []byte) {
	t.Helper()
	pemBytes, pubLine, err := generateSSHKeyMaterial("greenlight:" + name + "@test")
	if err != nil {
		t.Fatal(err)
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		t.Fatal(err)
	}
	return sshSessionKey{
		name:       name,
		secretName: sshSecretName(name),
		pub:        pub,
		comment:    comment,
	}, pemBytes
}

// startPipeAgent serves an sshAgentServer over an in-memory pipe and returns
// the client side, mirroring how ssh drives the session socket.
func startPipeAgent(t *testing.T, srv *sshAgentServer) agent.ExtendedAgent {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	go agent.ServeAgent(srv, serverConn)
	return agent.NewClient(clientConn)
}

// TestSSHAgentListSign drives List and Sign through the real agent wire
// protocol (agent.NewClient), the acceptance criterion for the socket.
func TestSSHAgentListSign(t *testing.T) {
	key, pemBytes := newTestAgentKey(t, "staging")
	fetches := 0
	srv := &sshAgentServer{
		keys: []sshSessionKey{key},
		fetchPEM: func(secretName string) ([]byte, error) {
			fetches++
			if secretName != "SSH_KEY_STAGING" {
				return nil, fmt.Errorf("unexpected secret %q", secretName)
			}
			return append([]byte(nil), pemBytes...), nil
		},
	}
	client := startPipeAgent(t, srv)

	keys, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0].Comment != "greenlight:staging@test" {
		t.Fatalf("List = %+v, want the one staging key", keys)
	}

	data := []byte("challenge")
	sig, err := client.Sign(key.pub, data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := key.pub.Verify(data, sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
	// Decrypt-per-sign: a second sign fetches again (no caching).
	if _, err := client.Sign(key.pub, data); err != nil {
		t.Fatalf("second Sign: %v", err)
	}
	if fetches != 2 {
		t.Errorf("fetches = %d, want 2 (decrypt per sign, no cache)", fetches)
	}
}

// TestSSHAgentSignFailsClosed: a missing/expired secret at sign time returns
// an error over the wire; ssh fails closed.
func TestSSHAgentSignFailsClosed(t *testing.T) {
	key, _ := newTestAgentKey(t, "gone")
	srv := &sshAgentServer{
		keys:     []sshSessionKey{key},
		fetchPEM: func(string) ([]byte, error) { return nil, errors.New("not_found") },
	}
	client := startPipeAgent(t, srv)
	if _, err := client.Sign(key.pub, []byte("x")); err == nil {
		t.Error("Sign with a missing secret must fail")
	}

	// A key the agent doesn't serve is also refused.
	other, _ := newTestAgentKey(t, "other")
	if _, err := client.Sign(other.pub, []byte("x")); err == nil {
		t.Error("Sign with an unserved key must fail")
	}
}

// TestSSHAgentReadOnly: every mutating op is rejected through the real wire
// protocol — the agent cannot be loaded, drained, or locked by the child.
func TestSSHAgentReadOnly(t *testing.T) {
	key, pemBytes := newTestAgentKey(t, "ro")
	srv := &sshAgentServer{
		keys:     []sshSessionKey{key},
		fetchPEM: func(string) ([]byte, error) { return append([]byte(nil), pemBytes...), nil },
	}
	client := startPipeAgent(t, srv)

	if err := client.RemoveAll(); err == nil {
		t.Error("RemoveAll must be rejected")
	}
	if err := client.Remove(key.pub); err == nil {
		t.Error("Remove must be rejected")
	}
	if err := client.Lock([]byte("pw")); err == nil {
		t.Error("Lock must be rejected")
	}
	if err := client.Unlock([]byte("pw")); err == nil {
		t.Error("Unlock must be rejected")
	}
	if _, err := client.Extension("session-bind@openssh.com", nil); err == nil {
		t.Error("Extension must be rejected")
	}
	// Add requires a raw private key; generate a throwaway.
	_, addPEM := newTestAgentKey(t, "added")
	addSigner, err := ssh.ParseRawPrivateKey(addPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Add(agent.AddedKey{PrivateKey: addSigner}); err == nil {
		t.Error("Add must be rejected")
	}
	// The read path still works after the rejected mutations.
	if keys, err := client.List(); err != nil || len(keys) != 1 {
		t.Errorf("List after rejected mutations = %v, %v", keys, err)
	}
}

// TestStartSessionSSHAgent exercises the real Unix socket: 0600 mode, live
// List/Sign, and cleanup removing the socket.
func TestStartSessionSSHAgent(t *testing.T) {
	// A short TMPDIR: t.TempDir() embeds the test name and can push the
	// socket path past sun_path's 104-byte cap — the very limit the short
	// "gl-ssh-<8>.sock" name exists to respect.
	tmp, err := os.MkdirTemp("/tmp", "gl-ssh-t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	key, pemBytes := newTestAgentKey(t, "sock")
	sockPath, cleanup, err := startSessionSSHAgent("0123456789abcdef", []sshSessionKey{key},
		func(string) ([]byte, error) { return append([]byte(nil), pemBytes...), nil })
	if err != nil {
		t.Fatalf("startSessionSSHAgent: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Error("not a socket")
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("socket mode = %o, want 0600", perm)
	}
	if base := filepath.Base(sockPath); base != "gl-ssh-01234567.sock" {
		t.Errorf("socket name = %q, want truncated relay id", base)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := agent.NewClient(conn)
	keys, err := client.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("List over socket = %v, %v", keys, err)
	}
	data := []byte("over-the-socket")
	sig, err := client.Sign(key.pub, data)
	if err != nil {
		t.Fatalf("Sign over socket: %v", err)
	}
	if err := key.pub.Verify(data, sig); err != nil {
		t.Errorf("socket signature does not verify: %v", err)
	}

	cleanup()
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("cleanup must remove the socket")
	}
}

// TestSSHAgentSockPathFallback: a TMPDIR deep enough to blow sun_path's
// 104-byte budget falls back to /tmp so the bind never fails on path length.
func TestSSHAgentSockPathFallback(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	if got := sshAgentSockPath("0123456789abcdef"); got != "/tmp/gl-ssh-01234567.sock" {
		t.Errorf("short TMPDIR path = %q", got)
	}
	deep := "/tmp/" + strings.Repeat("d", 120)
	t.Setenv("TMPDIR", deep)
	if got := sshAgentSockPath("0123456789abcdef"); got != "/tmp/gl-ssh-01234567.sock" {
		t.Errorf("deep TMPDIR must fall back to /tmp, got %q", got)
	}
}

// TestResolveSSHSession covers the session-start resolution matrix: off ⇒
// zero value; on with missing secret or missing .pub ⇒ entry skipped, never
// a failure; on with both present ⇒ served.
func TestResolveSSHSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, greenlightDirName())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	writeConfig := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writePub := func(name string) sshSessionKey {
		_, pubLine, err := generateSSHKeyMaterial("greenlight:" + name + "@test")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "ssh"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ssh", name+".pub"), []byte(pubLine+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		pub, comment, _, _, _ := ssh.ParseAuthorizedKey([]byte(pubLine))
		return sshSessionKey{name: name, secretName: sshSecretName(name), pub: pub, comment: comment}
	}

	// Isolation off: zero value regardless of ssh_keys.
	writeConfig("ssh_keys=SSH_KEY_A\n")
	if st := resolveSSHSession("proj", map[string]bool{"SSH_KEY_A": true}); st.isolated || st.serving() {
		t.Errorf("isolation off must resolve to the zero state, got %+v", st)
	}

	// On, no keys configured: isolated, not serving.
	writeConfig("ssh_isolation=on\n")
	if st := resolveSSHSession("proj", nil); !st.isolated || st.serving() {
		t.Errorf("on+no-keys must be isolated and not serving, got %+v", st)
	}

	// On, key configured but secret missing (nil present set — WS down): skip,
	// and the skip is recorded (#292) so the caller can tell "configured but
	// unresolvable" apart from "never configured".
	writePub("a")
	writeConfig("ssh_isolation=on\nssh_keys=SSH_KEY_A\n")
	if st := resolveSSHSession("proj", nil); st.serving() {
		t.Errorf("missing secret must be skipped, got %+v", st)
	} else if len(st.skipped) != 1 || st.skipped[0] != "SSH_KEY_A" {
		t.Errorf("skipped = %v, want [SSH_KEY_A]", st.skipped)
	}

	// On, secret present but no .pub: skip, also recorded.
	writeConfig("ssh_isolation=on\nssh_keys=SSH_KEY_B\n")
	if st := resolveSSHSession("proj", map[string]bool{"SSH_KEY_B": true}); st.serving() {
		t.Errorf("missing .pub must be skipped, got %+v", st)
	} else if len(st.skipped) != 1 || st.skipped[0] != "SSH_KEY_B" {
		t.Errorf("skipped = %v, want [SSH_KEY_B]", st.skipped)
	}

	// On, both present: served. A second unresolvable entry is skipped
	// without dropping the good one, and is still recorded in skipped even
	// though the session overall serves a key.
	writeConfig("ssh_isolation=on\nssh_keys=SSH_KEY_A, SSH_KEY_MISSING\n")
	st := resolveSSHSession("proj", map[string]bool{"SSH_KEY_A": true})
	if !st.serving() || len(st.keys) != 1 || st.keys[0].name != "a" {
		t.Fatalf("resolvable key must be served, got %+v", st)
	}
	if names := st.keyNames(); len(names) != 1 || names[0] != "a" {
		t.Errorf("keyNames = %v", names)
	}
	if len(st.skipped) != 1 || st.skipped[0] != "SSH_KEY_MISSING" {
		t.Errorf("skipped = %v, want [SSH_KEY_MISSING]", st.skipped)
	}
}
