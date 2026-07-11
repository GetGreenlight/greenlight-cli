//go:build darwin || linux

package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// This file implements the per-session ssh-agent for isolated sessions
// (#250, Phase 1b of docs/ssh-isolation-spec.md). When ssh_isolation is on
// and the session's ssh_keys resolve to at least one stored key, the daemon
// serves a read-only ssh-agent on a per-session Unix socket and points the
// child's SSH_AUTH_SOCK at it (via the exportEnvs overlay — after the #249
// strip). The agent exposes only the ssh-agent wire protocol, which has no
// "export private key" operation: List returns public keys parsed from the
// on-disk .pub files, and Sign decrypts the named secret on demand inside
// the daemon process, signs the challenge, and zeroes the plaintext.
// Custody invariant: plaintext key material exists only transiently inside
// the daemon; the agent process never sees it — not in env, not on disk,
// not over the socket.

// sshSessionKey is one resolved key served by a session's ssh-agent.
type sshSessionKey struct {
	name       string        // short name, e.g. "staging"
	secretName string        // stored secret name, e.g. "SSH_KEY_STAGING"
	pub        ssh.PublicKey // parsed from the on-disk .pub
	comment    string        // comment from the .pub line
}

// sshSession is the single session-start resolution of ssh_isolation +
// ssh_keys (#250). One struct drives the env strip, the session socket, and
// the system prompt so they can't diverge. Zero value = isolation off,
// byte-for-byte today's behavior.
type sshSession struct {
	isolated bool
	keys     []sshSessionKey
	// skipped names the ssh_keys entries that were configured but could not be
	// resolved (missing secret or missing/unparseable local .pub) — see
	// resolveSSHSession. Distinguishing "configured but failed" from "never
	// configured" is what issue #292's silent-skip fix surfaces to the agent.
	skipped []string
}

// serving reports whether the session serves an ssh-agent: isolated with at
// least one resolved key. Isolated with zero keys = strip only, no socket
// (fail closed; #249 behavior stands).
func (s sshSession) serving() bool {
	return s.isolated && len(s.keys) > 0
}

// keyNames returns the short names for the system-prompt line.
func (s sshSession) keyNames() []string {
	names := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		names = append(names, k.name)
	}
	return names
}

// resolveSSHSession resolves the session's SSH state once at session start
// (same rule as the shims and the #249 bool it widens: a mid-session config
// flip has no effect on live sessions). A configured ssh_keys entry is kept
// only when both its stored secret and its on-disk .pub are present; the
// rest are skipped with a log line, never a session failure. present is the
// device's stored secret names (nil when the daemon WS is unavailable, which
// fails closed to zero keys).
func resolveSSHSession(project string, present map[string]bool) sshSession {
	st := sshSession{isolated: sshIsolationEnabled(project)}
	if !st.isolated {
		return st
	}
	for _, secretName := range sshConfiguredKeys(project) {
		if !present[secretName] {
			log.Printf("ssh-agent: skipping %s: no stored secret", secretName)
			st.skipped = append(st.skipped, secretName)
			continue
		}
		short := sshShortName(secretName)
		pub, comment, err := loadSSHPublicKey(short)
		if err != nil {
			log.Printf("ssh-agent: skipping %s: %v", secretName, err)
			st.skipped = append(st.skipped, secretName)
			continue
		}
		st.keys = append(st.keys, sshSessionKey{
			name:       short,
			secretName: secretName,
			pub:        pub,
			comment:    comment,
		})
	}
	return st
}

// loadSSHPublicKey reads and parses ~/.greenlight/ssh/<name>.pub.
func loadSSHPublicKey(name string) (ssh.PublicKey, string, error) {
	path, err := sshPubPath(name)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return pub, comment, nil
}

// errSSHAgentReadOnly is returned for every mutating ssh-agent operation:
// the session agent serves exactly the keys the user configured, nothing
// else, and cannot be locked into a different state by the agent process.
var errSSHAgentReadOnly = errors.New("greenlight ssh-agent is read-only")

// sshAgentServer implements agent.ExtendedAgent over the session's resolved
// key set. fetchPEM returns the decrypted OpenSSH PEM private key for a
// secret name; in production it is Daemon.sshSecretPlaintext (an in-process
// daemon-WS secrets_get + decrypt — NOT the client-side fetchAndDecrypt,
// which dials the daemon's own IPC socket and would deadlock). Decrypt is
// per-sign with no caching: a missing/expired secret at sign time returns an
// error and ssh fails closed.
type sshAgentServer struct {
	keys     []sshSessionKey
	fetchPEM func(secretName string) ([]byte, error)
}

func (a *sshAgentServer) List() ([]*agent.Key, error) {
	out := make([]*agent.Key, 0, len(a.keys))
	for _, k := range a.keys {
		out = append(out, &agent.Key{
			Format:  k.pub.Type(),
			Blob:    k.pub.Marshal(),
			Comment: k.comment,
		})
	}
	return out, nil
}

func (a *sshAgentServer) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.SignWithFlags(key, data, 0)
}

func (a *sshAgentServer) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	// The RSA SHA-2 flags are meaningless for ed25519 (the only key type
	// keygen produces); reject rather than sign with an ambiguous algorithm.
	if flags != 0 {
		return nil, fmt.Errorf("unsupported signature flags %d", flags)
	}
	want := key.Marshal()
	for _, k := range a.keys {
		if string(k.pub.Marshal()) != string(want) {
			continue
		}
		pemBytes, err := a.fetchPEM(k.secretName)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", k.secretName, err)
		}
		signer, err := ssh.ParsePrivateKey(pemBytes)
		zeroBytes(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", k.secretName, err)
		}
		return signer.Sign(rand.Reader, data)
	}
	return nil, errors.New("key not found")
}

func (a *sshAgentServer) Add(key agent.AddedKey) error   { return errSSHAgentReadOnly }
func (a *sshAgentServer) Remove(key ssh.PublicKey) error { return errSSHAgentReadOnly }
func (a *sshAgentServer) RemoveAll() error               { return errSSHAgentReadOnly }
func (a *sshAgentServer) Lock(passphrase []byte) error   { return errSSHAgentReadOnly }
func (a *sshAgentServer) Unlock(passphrase []byte) error { return errSSHAgentReadOnly }
func (a *sshAgentServer) Signers() ([]ssh.Signer, error) { return nil, errSSHAgentReadOnly }
func (a *sshAgentServer) Extension(extensionType string, contents []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}

// sshSecretPlaintext fetches and decrypts a secret from inside the daemon
// process, over the in-process daemon WS (presentSecretNames shape). No
// OAuth-refresh branch — SSH keys don't refresh; any error fails closed.
func (d *Daemon) sshSecretPlaintext(secretName string) ([]byte, error) {
	if d.daemonWS == nil {
		return nil, errors.New("daemon WebSocket not connected")
	}
	rid, err := newRequestID()
	if err != nil {
		return nil, err
	}
	raw, err := d.daemonWS.SendRequest("secrets_get", rid, map[string]interface{}{
		"request_id": rid,
		"key":        secretName,
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Ciphertext string `json:"ciphertext"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	if resp.Ciphertext == "" {
		return nil, errors.New("empty ciphertext")
	}
	priv, err := loadPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("load key: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(resp.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	return decryptSecret(priv, blob)
}

// sshAgentSockPath returns the per-session agent socket path. Short on
// purpose — macOS caps sun_path at 104 bytes, so like the interpose socket
// we use a truncated relay ID, and a $TMPDIR deep enough to blow the budget
// anyway falls back to /tmp (the interpose socket's home).
func sshAgentSockPath(relayID string) string {
	id := relayID
	if len(id) > 8 {
		id = id[:8]
	}
	name := "gl-ssh-" + id + ".sock"
	path := filepath.Join(os.TempDir(), name)
	if len(path) > 100 {
		path = filepath.Join("/tmp", name)
	}
	return path
}

// startSessionSSHAgent listens on the session's agent socket (mode 0600) and
// serves each accepted conn with agent.ServeAgent. Returns the socket path
// and a cleanup func that closes the listener and removes the socket
// (invoked from Session.cleanup, same lifecycle as the CLI shim dir).
func startSessionSSHAgent(relayID string, keys []sshSessionKey, fetchPEM func(string) ([]byte, error)) (string, func(), error) {
	sockPath := sshAgentSockPath(relayID)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		// A stale socket from a crashed prior session with the same relay
		// prefix: if nobody is listening, remove and retry once.
		if conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond); dialErr != nil {
			os.Remove(sockPath)
			listener, err = net.Listen("unix", sockPath)
		} else {
			conn.Close()
		}
		if err != nil {
			return "", nil, fmt.Errorf("ssh-agent socket %s: %w", sockPath, err)
		}
	}
	// Tighten before the path is handed to anything: the ssh-agent protocol
	// exposes sign, and like every standard ssh-agent the socket is the
	// user's to reach — but not other users'.
	if err := os.Chmod(sockPath, 0600); err != nil {
		listener.Close()
		os.Remove(sockPath)
		return "", nil, fmt.Errorf("chmod ssh-agent socket: %w", err)
	}

	srv := &sshAgentServer{keys: keys, fetchPEM: fetchPEM}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed — session cleanup
			}
			go func() {
				defer conn.Close()
				if err := agent.ServeAgent(srv, conn); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					log.Printf("ssh-agent: serve: %v", err)
				}
			}()
		}
	}()

	cleanup := func() {
		listener.Close()
		os.Remove(sockPath)
	}
	return sockPath, cleanup, nil
}
