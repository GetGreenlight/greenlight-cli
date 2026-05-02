//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runRun is the entry point for `greenlight run`.
//
// Usage: greenlight run [-e KEY[=ENV_NAME]]... [--] COMMAND [ARGS...]
//
// Each `-e KEY` injects the named secret into the child process environment as
// `KEY` (default) or `ENV_NAME` if remapped. The wrapper scrubs all encoded
// forms of every injected secret from the child's stdout/stderr before they
// reach the wrapper's own stdio.
func runRun(args []string) {
	if len(args) == 0 {
		runUsage()
		os.Exit(1)
	}

	type spec struct {
		envName string
		secret  string
	}
	var injects []spec
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-e", a == "--env":
			if i+1 >= len(args) {
				dieErr(fmt.Errorf("-e requires an argument"))
			}
			s := parseEnvSpec(args[i+1])
			injects = append(injects, spec{envName: s.envName, secret: s.secret})
			i += 2
		case a == "--":
			i++
			goto rest
		case a == "-h", a == "--help":
			runUsage()
			os.Exit(0)
		case strings.HasPrefix(a, "-"):
			dieErr(fmt.Errorf("unknown flag %q", a))
		default:
			goto rest
		}
	}
rest:
	cmdArgs := args[i:]
	if len(cmdArgs) == 0 {
		dieErr(fmt.Errorf("missing command"))
	}

	for _, spc := range injects {
		if spc.envName == "" || spc.envName == "PATH" {
			dieErr(fmt.Errorf("refusing to inject into env var %q", spc.envName))
		}
	}

	// Decrypt secrets up front. Refuse to run with partial env if any are
	// missing — partial injection is a footgun.
	plaintexts := make(map[string][]byte) // env_name → plaintext
	for _, spc := range injects {
		val, err := fetchAndDecrypt(spc.secret)
		if err != nil {
			dieErr(fmt.Errorf("secret %s: %w", spc.secret, err))
		}
		if len(val) < 8 {
			dieErr(fmt.Errorf("secret %s is too short to safely scrub (need >= 8 bytes)", spc.secret))
		}
		plaintexts[spc.envName] = val
	}

	// Build forms for scrubbing.
	var forms [][]byte
	for _, v := range plaintexts {
		forms = append(forms, encodingsOf(v)...)
	}
	forms = dedupeForms(forms)

	// Build child env: copy current env minus injected names, then add.
	env := os.Environ()
	skip := map[string]bool{}
	for k := range plaintexts {
		skip[k] = true
	}
	filtered := env[:0]
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			if skip[kv[:eq]] {
				continue
			}
		}
		filtered = append(filtered, kv)
	}
	for k, v := range plaintexts {
		filtered = append(filtered, k+"="+string(v))
	}

	bin, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		dieErr(fmt.Errorf("not found: %s", cmdArgs[0]))
	}

	cmd := exec.Command(bin, cmdArgs[1:]...)
	cmd.Env = filtered
	cmd.Stdin = os.Stdin

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		dieErr(err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		dieErr(err)
	}

	if err := cmd.Start(); err != nil {
		dieErr(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := scrub(stdoutPipe, os.Stdout, forms); err != nil && err != io.EOF {
			fmt.Fprintf(os.Stderr, "greenlight: stdout scrub error: %v\n", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := scrub(stderrPipe, os.Stderr, forms); err != nil && err != io.EOF {
			fmt.Fprintf(os.Stderr, "greenlight: stderr scrub error: %v\n", err)
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	// Best-effort zero of plaintexts.
	for k, v := range plaintexts {
		for i := range v {
			v[i] = 0
		}
		delete(plaintexts, k)
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
			os.Exit(1)
		}
		dieErr(waitErr)
	}
}

func runUsage() {
	fmt.Fprintln(os.Stderr, `Usage: greenlight run [-e KEY[=ENV_NAME]]... [--] COMMAND [ARGS...]

Decrypts named secrets locally and injects them into the child process
environment. The child's stdout/stderr is scrubbed of all encoded forms of
every injected secret before being forwarded.

Examples:
  greenlight run -e GITHUB_TOKEN -- gh issue list
  greenlight run -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -- aws s3 ls
  greenlight run -e GH=GITHUB_TOKEN -- bash -c 'echo $GH'`)
}

type envSpec struct {
	envName string
	secret  string
}

// parseEnvSpec parses "KEY" or "ENV_NAME=KEY".
func parseEnvSpec(s string) envSpec {
	if eq := strings.IndexByte(s, '='); eq > 0 {
		return envSpec{envName: s[:eq], secret: s[eq+1:]}
	}
	return envSpec{envName: s, secret: s}
}

// fetchAndDecrypt retrieves and decrypts a secret. If the secret is expired
// and a *_REFRESH_TOKEN exists, it triggers an OAuth refresh and retries.
func fetchAndDecrypt(key string) ([]byte, error) {
	raw, err := daemonWSRequest("secrets_get", map[string]interface{}{
		"key": key,
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Ciphertext string `json:"ciphertext"`
		ExpiresAt  string `json:"expires_at"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error == "not_found" {
		return nil, fmt.Errorf("not found")
	}
	if resp.Error == "expired" {
		// Try OAuth refresh if this looks like an access token.
		if strings.HasSuffix(key, "_ACCESS_TOKEN") {
			if newPlain, refreshErr := refreshAccessToken(key); refreshErr == nil {
				return newPlain, nil
			} else {
				return nil, fmt.Errorf("expired; refresh failed: %w", refreshErr)
			}
		}
		return nil, fmt.Errorf("expired")
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	if resp.Ciphertext == "" {
		return nil, fmt.Errorf("empty ciphertext")
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

// encodingsOf returns deduplicated byte forms of a secret used by scrub.
// Covers plaintext, lowercase hex, uppercase hex, base64 (standard + URL-safe,
// padded + unpadded), and URL-encoded.
func encodingsOf(secret []byte) [][]byte {
	formsSet := map[string]struct{}{
		string(secret):                                {},
		hex.EncodeToString(secret):                    {},
		strings.ToUpper(hex.EncodeToString(secret)):   {},
		base64.StdEncoding.EncodeToString(secret):     {},
		base64.RawStdEncoding.EncodeToString(secret):  {},
		base64.URLEncoding.EncodeToString(secret):     {},
		base64.RawURLEncoding.EncodeToString(secret):  {},
		url.QueryEscape(string(secret)):               {},
	}
	out := make([][]byte, 0, len(formsSet))
	for s := range formsSet {
		out = append(out, []byte(s))
	}
	return out
}

func dedupeForms(forms [][]byte) [][]byte {
	seen := map[string]struct{}{}
	out := make([][]byte, 0, len(forms))
	for _, f := range forms {
		if len(f) == 0 {
			continue
		}
		k := string(f)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, f)
	}
	return out
}

// scrub copies src→dst, replacing every occurrence of any form in `forms`
// with `***`. A carry buffer of `maxFormLen-1` bytes prevents a form that
// straddles two reads from slipping through.
func scrub(src io.Reader, dst io.Writer, forms [][]byte) error {
	if len(forms) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	overlap := len(forms[0]) - 1

	buf := make([]byte, 64*1024)
	var carry []byte
	redaction := []byte("***")

	for {
		n, err := src.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			for _, f := range forms {
				window = bytes.ReplaceAll(window, f, redaction)
			}
			if len(window) > overlap {
				if _, werr := dst.Write(window[:len(window)-overlap]); werr != nil {
					return werr
				}
				carry = append(carry[:0], window[len(window)-overlap:]...)
			} else {
				carry = append(carry[:0], window...)
			}
		}
		if err == io.EOF {
			for _, f := range forms {
				carry = bytes.ReplaceAll(carry, f, redaction)
			}
			_, werr := dst.Write(carry)
			return werr
		}
		if err != nil {
			return err
		}
	}
}
