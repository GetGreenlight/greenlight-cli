//go:build darwin || linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

func runSecrets(args []string) {
	if len(args) == 0 {
		secretsUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "init":
		rotate := false
		for _, a := range args[1:] {
			if a == "--rotate" {
				rotate = true
			}
		}
		secretsInit(rotate)
	case "list", "ls":
		secretsList()
	case "set":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight secrets set KEY")
			os.Exit(1)
		}
		secretsSet(args[1])
	case "rm", "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight secrets rm KEY")
			os.Exit(1)
		}
		secretsRm(args[1])
	case "help", "--help", "-h":
		secretsUsage()
	default:
		fmt.Fprintf(os.Stderr, "greenlight secrets: unknown command %q\n\n", args[0])
		secretsUsage()
		os.Exit(1)
	}
}

func secretsUsage() {
	fmt.Fprintln(os.Stderr, `Usage: greenlight secrets <command> [args]

Commands:
  init [--rotate]   Generate encryption keypair and upload public half
  list              List secret names and expiries
  set KEY           Store a secret (reads value from stdin, no echo)
  rm KEY            Delete a secret`)
}

// secretsInit generates a fresh X25519 keypair, writes the private half to
// ~/.greenlight/key, and registers the public half with the server.
func secretsInit(rotate bool) {
	// Refuse to overwrite without --rotate so we don't silently destroy data.
	kp, err := keyPath()
	if err != nil {
		dieErr(err)
	}
	if _, err := os.Stat(kp); err == nil && !rotate {
		fmt.Fprintf(os.Stderr, "key already exists at %s; pass --rotate to replace it\n", kp)
		os.Exit(1)
	}

	priv, err := generateKeypair()
	if err != nil {
		dieErr(err)
	}
	if err := savePrivateKey(priv, rotate); err != nil {
		dieErr(err)
	}

	pubB64 := base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())

	// First attempt: server will reply needs_confirm if rotation would orphan secrets.
	resp, err := pubkeyPut(pubB64, false)
	if err != nil {
		// Roll back the local key so we don't leave it desynced from the server.
		_ = removePrivateKey()
		dieErr(err)
	}

	switch resp.Status {
	case "ok":
		if resp.Wiped > 0 {
			fmt.Fprintf(os.Stderr, "keypair installed; wiped %d stale secret(s) from server\n", resp.Wiped)
		} else {
			fmt.Fprintf(os.Stderr, "keypair installed (fingerprint %s)\n", keyFingerprint(priv.PublicKey().Bytes()))
		}
	case "needs_confirm":
		fmt.Fprintf(os.Stderr, "rotation will wipe %d existing secret(s) on the server.\n", resp.ExistingCount)
		fmt.Fprintf(os.Stderr, "those secrets are encrypted to the old key, which is now gone, so they are unrecoverable.\n")
		fmt.Fprint(os.Stderr, "continue? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			_ = removePrivateKey()
			fmt.Fprintln(os.Stderr, "aborted; private key removed")
			os.Exit(1)
		}
		resp2, err := pubkeyPut(pubB64, true)
		if err != nil {
			_ = removePrivateKey()
			dieErr(err)
		}
		if resp2.Status != "ok" {
			_ = removePrivateKey()
			dieErr(fmt.Errorf("rotation failed: %s", resp2.Error))
		}
		fmt.Fprintf(os.Stderr, "keypair installed; wiped %d secret(s)\n", resp2.Wiped)
	default:
		_ = removePrivateKey()
		dieErr(fmt.Errorf("pubkey upload failed: %s", resp.Error))
	}
}

func secretsList() {
	raw, err := daemonWSRequest("secrets_list", map[string]interface{}{}, 30*time.Second)
	if err != nil {
		dieErr(err)
	}
	var resp struct {
		Secrets []struct {
			KeyName   string  `json:"key_name"`
			ExpiresAt *string `json:"expires_at"`
		} `json:"secrets"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		dieErr(err)
	}
	if resp.Error != "" {
		dieErr(fmt.Errorf("%s", resp.Error))
	}
	if len(resp.Secrets) == 0 {
		fmt.Fprintln(os.Stderr, "(no secrets)")
		return
	}
	now := time.Now().UTC()
	for _, s := range resp.Secrets {
		expiry := "never"
		if s.ExpiresAt != nil && *s.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, *s.ExpiresAt); err == nil {
				if now.After(t) {
					expiry = "EXPIRED"
				} else {
					expiry = "expires " + humanDuration(t.Sub(now))
				}
			} else {
				expiry = *s.ExpiresAt
			}
		}
		fmt.Printf("%-32s  %s\n", s.KeyName, expiry)
	}
}

func secretsSet(key string) {
	if !validSecretKey(key) {
		dieErr(fmt.Errorf("invalid key name; allowed chars: a-z A-Z 0-9 _ . -"))
	}

	plain, err := readSecretValue()
	if err != nil {
		dieErr(err)
	}
	if len(plain) == 0 {
		dieErr(fmt.Errorf("empty value"))
	}

	priv, err := loadPrivateKey()
	if err != nil {
		dieErr(fmt.Errorf("load private key: %w (run `greenlight secrets init` first)", err))
	}
	ct, err := encryptSecret(priv.PublicKey(), plain)
	if err != nil {
		dieErr(err)
	}

	raw, err := daemonWSRequest("secrets_put", map[string]interface{}{
		"key":        key,
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
	fmt.Fprintf(os.Stderr, "stored %s\n", key)
}

func secretsRm(key string) {
	raw, err := daemonWSRequest("secrets_delete", map[string]interface{}{
		"key": key,
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
	fmt.Fprintf(os.Stderr, "removed %s\n", key)
}

// pubkeyPutResponse is the parsed shape of pubkey_put_response.
type pubkeyPutResponse struct {
	Status        string `json:"status"`
	Error         string `json:"error"`
	Wiped         int    `json:"wiped"`
	ExistingCount int    `json:"existing_count"`
}

func pubkeyPut(publicKeyB64 string, confirm bool) (*pubkeyPutResponse, error) {
	body := map[string]interface{}{
		"public_key": publicKeyB64,
	}
	if confirm {
		body["confirm"] = true
	}
	raw, err := daemonWSRequest("pubkey_put", body, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var resp pubkeyPutResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// readSecretValue reads a secret value from stdin: hidden prompt if interactive,
// raw read if piped. Trailing single newline is trimmed in pipe mode.
func readSecretValue() ([]byte, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "value: ")
		v, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return v, err
	}
	v, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(string(v), "\n")), nil
}

// validSecretKey mirrors the server's regex.
func validSecretKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "in <1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("in %dh", int(d.Hours()))
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

func dieErr(err error) {
	fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
	os.Exit(1)
}
