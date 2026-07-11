//go:build darwin || linux

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshSecretPrefix is the discovery convention for agent SSH keys stored as
// greenlight secrets (like ${PROVIDER}_ACCESS_TOKEN for OAuth): keygen stores
// the private key as SSH_KEY_<NAME>, and the apps filter the secrets list on
// this prefix for the ssh_keys picker.
const sshSecretPrefix = "SSH_KEY_"

// runSSH is the entry point for `greenlight ssh` (#250): user-generated
// per-agent SSH keys, stored encrypted as ordinary secrets and served to
// isolated sessions by a greenlight-owned ssh-agent (see sshagent.go).
// Private keys are never printed, exported, or written to disk in plaintext
// by any subcommand.
func runSSH(args []string) {
	if len(args) == 0 {
		sshUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "keygen":
		sshKeygen(args[1:])
	case "pubkey":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight ssh pubkey NAME [--unrestricted]")
			os.Exit(1)
		}
		sshPubkeyCmd(args[1], sshHasFlag(args[2:], "--unrestricted"))
	case "list", "ls":
		sshList()
	case "rm", "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight ssh rm NAME")
			os.Exit(1)
		}
		sshRm(args[1])
	case "help", "--help", "-h":
		sshUsage()
	default:
		fmt.Fprintf(os.Stderr, "greenlight ssh: unknown command %q\n\n", args[0])
		sshUsage()
		os.Exit(1)
	}
}

func sshUsage() {
	fmt.Fprintln(os.Stderr, `Usage: greenlight ssh <command> [args]

Scoped SSH keys for isolated agent sessions (ssh_isolation=on). Keys are
generated locally, stored encrypted as secrets (SSH_KEY_<NAME>), and served
to sessions named in the ssh_keys config by a read-only greenlight ssh-agent
— the agent process never sees private key material.

Commands:
  keygen NAME [--comment C] [--unrestricted]
                 Generate an ed25519 keypair; prints the authorized_keys line
                 to paste on the hosts this agent may reach (hardened with
                 "restrict" unless --unrestricted)
  pubkey NAME    Re-print the authorized_keys line
  list           List keys with created-at, cross-checked against secrets
  rm NAME        Delete the stored secret and local public key`)
}

func sshHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// sshDir returns the directory holding local public keys
// (~/.greenlight/ssh). Public material only — 0644 files in a 0700 dir.
func sshDir() (string, error) {
	dir, err := greenlightDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh"), nil
}

// sshSecretName maps a keygen short name to its stored secret name:
// staging → SSH_KEY_STAGING.
func sshSecretName(name string) string {
	return sshSecretPrefix + strings.ToUpper(name)
}

// sshShortName is the inverse of sshSecretName: SSH_KEY_STAGING → staging.
// A configured ssh_keys entry without the prefix degrades to its own
// lowercase form so a hand-written entry still resolves a .pub.
func sshShortName(secretName string) string {
	return strings.ToLower(strings.TrimPrefix(secretName, sshSecretPrefix))
}

// sshPubPath returns the on-disk public key path for a short name.
func sshPubPath(name string) (string, error) {
	dir, err := sshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strings.ToLower(name)+".pub"), nil
}

// generateSSHKeyMaterial is the pure keygen core, separated for testability:
// generate an ed25519 keypair and return the OpenSSH PEM private key bytes
// and the public key line ("ssh-ed25519 AAAA… comment"). The caller owns
// zeroing the PEM bytes after use.
func generateSSHKeyMaterial(comment string) (pemBytes []byte, pubLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, "", fmt.Errorf("marshal private key: %w", err)
	}
	pemBytes = pem.EncodeToMemory(block)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		zeroBytes(pemBytes)
		return nil, "", fmt.Errorf("marshal public key: %w", err)
	}
	pubLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		pubLine += " " + comment
	}
	return pemBytes, pubLine, nil
}

// zeroBytes overwrites b in place. Defense-in-depth, not the custody
// invariant: Go can't scrub every internal copy the crypto layer makes —
// the invariant is that plaintext key material never leaves this process.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// authorizedKeysLine renders the ready-to-paste line: "restrict" hardened by
// default so a compromised agent key can't forward ports or agents on the
// target host; --unrestricted opts out.
func authorizedKeysLine(pubLine string, unrestricted bool) string {
	if unrestricted {
		return pubLine
	}
	return "restrict " + pubLine
}

// sshKeygen generates a named ed25519 keypair in-process: the private key
// (OpenSSH PEM) is encrypted client-side and stored as secret SSH_KEY_<NAME>;
// the public key is written to ~/.greenlight/ssh/<name>.pub. Refuses to
// overwrite an existing name — there is no silent rotation; rm first.
func sshKeygen(args []string) {
	var name, comment string
	unrestricted := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--comment":
			if i+1 >= len(args) {
				dieErr(fmt.Errorf("--comment requires an argument"))
			}
			comment = args[i+1]
			i++
		case "--unrestricted":
			unrestricted = true
		default:
			if strings.HasPrefix(args[i], "-") {
				dieErr(fmt.Errorf("unknown flag %q", args[i]))
			}
			if name != "" {
				dieErr(fmt.Errorf("unexpected argument %q", args[i]))
			}
			name = args[i]
		}
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Usage: greenlight ssh keygen NAME [--comment C] [--unrestricted]")
		os.Exit(1)
	}
	if !validSecretKey(name) {
		dieErr(fmt.Errorf("invalid key name; allowed chars: a-z A-Z 0-9 _ . -"))
	}

	secretName := sshSecretName(name)
	pubPath, err := sshPubPath(name)
	if err != nil {
		dieErr(err)
	}
	if _, err := os.Stat(pubPath); err == nil {
		dieErr(fmt.Errorf("key %q already exists (%s); run `greenlight ssh rm %s` first", name, pubPath, strings.ToLower(name)))
	}
	if present, err := sshStoredSecretNames(); err != nil {
		dieErr(err)
	} else if present[secretName] {
		dieErr(fmt.Errorf("secret %s already exists; run `greenlight ssh rm %s` first", secretName, strings.ToLower(name)))
	}

	// Encrypt before anything is stored: a failure here leaves no state.
	glPriv, err := loadPrivateKey()
	if err != nil {
		dieErr(fmt.Errorf("load private key: %w (run `greenlight secrets init` first)", err))
	}
	if comment == "" {
		hostname, _ := os.Hostname()
		comment = "greenlight:" + strings.ToLower(name) + "@" + hostname
	}
	pemBytes, pubLine, err := generateSSHKeyMaterial(comment)
	if err != nil {
		dieErr(err)
	}
	ct, err := encryptSecret(glPriv.PublicKey(), pemBytes)
	zeroBytes(pemBytes)
	if err != nil {
		dieErr(err)
	}

	raw, err := daemonWSRequest("secrets_put", map[string]interface{}{
		"key":        secretName,
		"ciphertext": base64.StdEncoding.EncodeToString(ct),
	}, 30*time.Second)
	if err != nil {
		dieErr(err)
	}
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		dieErr(err)
	}
	if resp.Status != "ok" {
		dieErr(fmt.Errorf("%s", resp.Error))
	}

	if err := os.MkdirAll(filepath.Dir(pubPath), 0700); err != nil {
		dieErr(fmt.Errorf("create ssh dir: %w", err))
	}
	if err := os.WriteFile(pubPath, []byte(pubLine+"\n"), 0644); err != nil {
		// Roll back the stored secret so we don't leave an orphan the session
		// resolver would skip anyway.
		_, _ = daemonWSRequest("secrets_delete", map[string]interface{}{"key": secretName}, 30*time.Second)
		dieErr(fmt.Errorf("write public key: %w", err))
	}

	fmt.Fprintf(os.Stderr, "stored %s; public key written to %s\n\n", secretName, pubPath)
	fmt.Fprintf(os.Stderr, "public key:\n")
	fmt.Printf("%s\n\n", pubLine)
	fmt.Fprintf(os.Stderr, "authorized_keys line (paste on each host this agent may reach):\n")
	fmt.Printf("%s\n", authorizedKeysLine(pubLine, unrestricted))
	fmt.Fprintf(os.Stderr, "\nserve it to sessions with: greenlight config set ssh_keys %s (requires ssh_isolation=on)\n", secretName)
}

// sshPubkeyCmd re-prints the authorized_keys line for a stored key.
func sshPubkeyCmd(name string, unrestricted bool) {
	pubPath, err := sshPubPath(name)
	if err != nil {
		dieErr(err)
	}
	data, err := os.ReadFile(pubPath)
	if err != nil {
		if os.IsNotExist(err) {
			dieErr(fmt.Errorf("no key named %q (run `greenlight ssh list`)", name))
		}
		dieErr(err)
	}
	fmt.Println(authorizedKeysLine(strings.TrimSpace(string(data)), unrestricted))
}

// sshStoredSecretNames returns the device's stored secret names via the
// daemon. Client-side counterpart of the daemon's presentSecretNames, but an
// error is surfaced rather than degraded — key management must not act on a
// stale view.
func sshStoredSecretNames() (map[string]bool, error) {
	raw, err := daemonWSRequest("secrets_list", map[string]interface{}{}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Secrets []struct {
			KeyName string `json:"key_name"`
		} `json:"secrets"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	present := make(map[string]bool, len(resp.Secrets))
	for _, s := range resp.Secrets {
		present[s.KeyName] = true
	}
	return present, nil
}

// sshList shows local keys (name + created-at from the .pub file) cross-
// checked against the stored secrets, flagging orphans in either direction:
// a .pub without its secret can't sign, and an SSH_KEY_* secret without a
// .pub can't be served (the session resolver skips both).
func sshList() {
	dir, err := sshDir()
	if err != nil {
		dieErr(err)
	}
	present, err := sshStoredSecretNames()
	if err != nil {
		dieErr(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		dieErr(err)
	}
	seen := map[string]bool{}
	var rows []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".pub")
		created := "?"
		if info, err := e.Info(); err == nil {
			created = info.ModTime().Format("2006-01-02")
		}
		secretName := sshSecretName(name)
		seen[secretName] = true
		note := ""
		if !present[secretName] {
			note = "  (secret missing — cannot sign; rm and re-keygen)"
		}
		rows = append(rows, fmt.Sprintf("%-24s  %s  %s%s", name, created, secretName, note))
	}
	// Secrets with no local .pub (e.g. keygen'd on another host).
	var orphans []string
	for s := range present {
		if strings.HasPrefix(s, sshSecretPrefix) && !seen[s] {
			orphans = append(orphans, s)
		}
	}
	sort.Strings(rows)
	sort.Strings(orphans)

	if len(rows) == 0 && len(orphans) == 0 {
		fmt.Fprintln(os.Stderr, "(no ssh keys — create one with `greenlight ssh keygen NAME`)")
		return
	}
	for _, r := range rows {
		fmt.Println(r)
	}
	for _, s := range orphans {
		fmt.Printf("%-24s  %s  %s  (no local .pub on this host — cannot serve here)\n", sshShortName(s), "?", s)
	}
}

// sshRm deletes the stored secret (riding the existing secret-deletion
// approval path) and removes the local .pub. Tolerates a missing secret so a
// half-created key can still be cleaned up.
func sshRm(name string) {
	secretName := sshSecretName(name)
	raw, err := daemonWSRequest("secrets_delete", map[string]interface{}{
		"key": secretName,
	}, 30*time.Second)
	if err != nil {
		dieErr(err)
	}
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		dieErr(err)
	}
	if resp.Status != "ok" && resp.Error != "not_found" {
		dieErr(fmt.Errorf("%s", resp.Error))
	}

	pubPath, err := sshPubPath(name)
	if err != nil {
		dieErr(err)
	}
	if err := os.Remove(pubPath); err != nil && !os.IsNotExist(err) {
		dieErr(err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", name)
}
