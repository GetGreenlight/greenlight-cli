//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

// shimSpec maps a known command to the greenlight secret(s) that authenticate
// it and the environment variable the tool reads that secret from.
//
// When the agent runs such a command bare (e.g. `gh issue list`), greenlight
// transparently re-runs it as `greenlight run -e ENV=SECRET -- gh issue list`:
// the secret is decrypted and injected into the child's env, the child's
// output is scrubbed of the secret, and the agent never learns a token was
// involved. The human still sees and approves the `greenlight run …` form on
// the phone.
type shimSpec struct {
	cmd     string // invocation name, e.g. "gh"
	envName string // env var the tool reads the token from, e.g. "GH_TOKEN"
}

// knownShims is the registry of commands greenlight can transparently wrap. The
// secret to inject is config-driven (see configuredShimOverrides) — there is no
// built-in candidate list, so a command is shimmed only when the user has
// configured a secret for it.
var knownShims = map[string]shimSpec{
	"gh":   {cmd: "gh", envName: "GH_TOKEN"},
	"glab": {cmd: "glab", envName: "GITLAB_TOKEN"},
}

// resolvedShim is a knownShims entry bound to the specific secret name that is
// actually present for this device.
type resolvedShim struct {
	cmd     string
	secret  string
	envName string
}

// activeShims returns the resolved shims to install for this session. It is
// driven entirely by config: override maps a shim command (e.g. "gh") to the
// configured secret name to inject (see configuredShimOverrides). A command is
// shimmed only when its override secret is actually stored — there is no
// built-in token fallback. present/override may be nil (returns nil). Result
// order is sorted by command for determinism.
func activeShims(present map[string]bool, override map[string]string) []resolvedShim {
	if len(present) == 0 || len(override) == 0 {
		return nil
	}
	var out []resolvedShim
	for _, name := range sortedKeys(knownShims) {
		spec := knownShims[name]
		if sec := override[spec.cmd]; sec != "" && present[sec] {
			out = append(out, resolvedShim{cmd: spec.cmd, secret: sec, envName: spec.envName})
		}
	}
	return out
}

// providerShimCmd maps a tickets provider to the shimmed CLI it authenticates.
// The configured shim_secret applies to this command. Empty if the provider has
// no associated shim.
func providerShimCmd(provider string) string {
	switch provider {
	case "github":
		return "gh"
	default:
		return ""
	}
}

// configuredShimOverrides resolves the secret to inject into the configured
// provider's CLI (e.g. gh). In order of preference it uses the CLI secret
// (shim_secret), then the API secret (tickets_secret) — and only a name that is
// actually stored (`present`). There is no built-in token fallback: if neither
// is configured and present, the command isn't shimmed. Returns nil in that
// case.
func configuredShimOverrides(project string, present map[string]bool) map[string]string {
	provider := resolveConfig(project, configKeyTicketsProvider)
	if provider == "" {
		provider = defaultTicketsProvider
	}
	cmd := providerShimCmd(provider)
	if cmd == "" {
		return nil
	}
	for _, key := range []string{configKeyShimSecret, configKeyTicketsSecret} {
		if name := resolveConfig(project, key); name != "" && present[name] {
			return map[string]string{cmd: name}
		}
	}
	return nil
}

func sortedKeys(m map[string]shimSpec) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Small map; insertion sort keeps it dependency-free.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// shimsEnabled reports whether command shims are active. Default on; disabled
// only when the config explicitly sets shims_enabled=false.
func shimsEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(readConfigValue("shims_enabled")), "false")
}

// presentSecretNames returns the set of secret key names stored for this
// device, queried over the daemon WebSocket. Best-effort: returns nil if the
// daemon WS is unavailable or the request fails, so shimming silently degrades
// to the agent's own credentials rather than breaking the session.
func (d *Daemon) presentSecretNames() map[string]bool {
	if d.daemonWS == nil {
		return nil
	}
	rid, err := newRequestID()
	if err != nil {
		return nil
	}
	raw, err := d.daemonWS.SendRequest("secrets_list", rid,
		map[string]interface{}{"request_id": rid}, 5*time.Second)
	if err != nil {
		log.Printf("shims: secrets_list failed: %v", err)
		return nil
	}
	var resp struct {
		Secrets []struct {
			KeyName string `json:"key_name"`
		} `json:"secrets"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Error != "" {
		return nil
	}
	present := make(map[string]bool, len(resp.Secrets))
	for _, s := range resp.Secrets {
		present[s.KeyName] = true
	}
	return present
}

// installCommandShims symlinks each active shim command into the session bin
// dir, pointing back at the running greenlight binary so the agent's bare
// invocation resolves to the shim. Best-effort.
func installCommandShims(shimDir string, active []resolvedShim) {
	if shimDir == "" || len(active) == 0 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for _, rs := range active {
		link := filepath.Join(shimDir, rs.cmd)
		_ = os.Remove(link) // replace any stale link
		if err := os.Symlink(exe, link); err != nil {
			log.Printf("shims: symlink %s: %v", rs.cmd, err)
		}
	}
}

// --- display rewrite (daemon-side) ------------------------------------------

var (
	activeShimMu  sync.RWMutex
	activeShimReg = map[string]resolvedShim{}
)

// setActiveShims records the active shims so the interpose handler can rewrite
// gated commands to their `greenlight run` form for display. Idempotent;
// secrets are device-scoped so all sessions on a daemon share one set.
func setActiveShims(active []resolvedShim) {
	activeShimMu.Lock()
	defer activeShimMu.Unlock()
	for _, rs := range active {
		activeShimReg[rs.cmd] = rs
	}
}

// rewriteShimCommand rewrites each bare shim invocation in a command to the
// `greenlight run -e ENV=SECRET -- <cmd>` form the PATH shim actually executes,
// so the phone shows the secret-injecting command. It parses the command with a
// real shell parser and wraps every simple command whose name is an active shim
// — at any nesting depth — preserving the surrounding structure:
//
//	head x | gh issue list  →  head x | greenlight run -e … -- gh issue list
//	gh a && gh b            →  greenlight run … -- gh a && greenlight run … -- gh b
//	echo $(gh issue list)   →  echo $(greenlight run … -- gh issue list)
//
// Returns cmd unchanged when nothing is a shim invocation or the command can't
// be parsed. This is display/rule-matching only — execution always goes through
// the PATH shim regardless, so even an unparseable command is still injected.
func rewriteShimCommand(cmd string) string {
	out, _ := rewriteShimCommandKeys(cmd)
	return out
}

// rewriteShimCommandKeys is rewriteShimCommand plus the loop-guard keys: for
// each wrapped invocation it returns the normalized "<basename> <args>" the
// shim's re-exec of the real binary will present, so the caller can pre-approve
// that re-exec and avoid a second prompt (see shim_guard.go).
func rewriteShimCommandKeys(cmd string) (string, []string) {
	activeShimMu.RLock()
	n := len(activeShimReg)
	activeShimMu.RUnlock()
	if n == 0 || strings.TrimSpace(cmd) == "" {
		return cmd, nil
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return cmd, nil // unparseable — leave alone
	}

	// Collect the byte offset of each shim command name. Inserting the
	// `greenlight run … -- ` prefix at the *name* position (not the CallExpr
	// start) keeps any leading env assignments — `FOO=bar gh …` becomes
	// `FOO=bar greenlight run … -- gh …`, which forwards FOO correctly.
	type insertion struct {
		offset uint
		prefix string
	}
	var inserts []insertion
	var keys []string
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, ok := bareWordLiteral(call.Args[0])
		if !ok || strings.Contains(name, "/") {
			return true // quoted/expanded word, or a path (bypasses the shim)
		}
		activeShimMu.RLock()
		rs, ok := activeShimReg[name]
		activeShimMu.RUnlock()
		if !ok {
			return true
		}
		inserts = append(inserts, insertion{
			offset: call.Args[0].Pos().Offset(),
			prefix: fmt.Sprintf("greenlight run -e %s=%s -- ", rs.envName, rs.secret),
		})
		// The shim re-execs this inner command (name + args, redirects excluded
		// since they live on the Stmt, not the CallExpr). Record its key.
		start := int(call.Args[0].Pos().Offset())
		end := int(call.Args[len(call.Args)-1].End().Offset())
		if start >= 0 && end <= len(cmd) && start < end {
			keys = append(keys, normalizeShimKey(cmd[start:end]))
		}
		return true
	})
	if len(inserts) == 0 {
		return cmd, nil
	}
	// Apply right-to-left so earlier offsets stay valid as we splice.
	sort.Slice(inserts, func(i, j int) bool { return inserts[i].offset > inserts[j].offset })
	out := cmd
	for _, ins := range inserts {
		o := int(ins.offset)
		if o < 0 || o > len(out) {
			continue
		}
		out = out[:o] + ins.prefix + out[o:]
	}
	return out, keys
}

// bareWordLiteral returns the literal value of a word that is a single unquoted
// literal (e.g. "gh"), and false for anything quoted, expanded, or concatenated
// — those don't name a bare command the PATH shim would intercept.
func bareWordLiteral(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) != 1 {
		return "", false
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	return lit.Value, true
}

// --- shim execution (child process) -----------------------------------------

// runShim is the multi-call entry point: greenlight invoked as a shimmed
// command (argv[0] == "gh" etc). It resolves the real binary, then injects the
// exact secret the daemon resolved for the display and re-runs the command
// through the `greenlight run` injection+scrub path.
//
// The secret is taken from the daemon's authoritative resolution (the same
// activeShimReg entry the phone was shown) rather than re-derived here, so the
// injected secret can never diverge from the approved command. If that secret
// can't be obtained or decrypted, the real binary runs unchanged (its own
// credentials) — we never substitute a different secret than was displayed.
func runShim(spec shimSpec, args []string) {
	real, rerr := resolveRealBinary(spec.cmd)

	var envName string
	var plain []byte
	if rs, ok := resolveShimFromDaemon(spec.cmd); ok {
		if v, err := fetchAndDecrypt(rs.secret); err == nil && len(v) >= 8 {
			envName, plain = rs.envName, v
		}
	}

	if rerr != nil {
		// The real command isn't installed — behave like the shell would.
		fmt.Fprintf(os.Stderr, "greenlight: %s: command not found\n", spec.cmd)
		os.Exit(127)
	}

	if plain == nil {
		// No usable secret — run the real binary unchanged (its own auth).
		execReal(real, args)
		return
	}

	runDecryptedChild(
		map[string][]byte{envName: plain},
		append([]string{real}, args...),
		func(p string) (string, error) { return p, nil },
	)
}

// resolveShimFromDaemon asks the running daemon for the active shim resolution
// for cmd — the same (secret, envName) the daemon used to rewrite the command
// for display. Returns false if there's no active shim or the daemon is
// unreachable. This is the authoritative source: the agent can't influence the
// daemon's reply, so the injected secret always matches the approved command.
func resolveShimFromDaemon(cmd string) (resolvedShim, bool) {
	resp, err := ipcExchange(ipcRequest{Type: "resolve_shim", Shim: cmd})
	if err != nil || resp.Type != "resolve_shim_response" || resp.ShimSecret == "" {
		return resolvedShim{}, false
	}
	return resolvedShim{cmd: cmd, secret: resp.ShimSecret, envName: resp.ShimEnv}, true
}

// lookupActiveShim returns the daemon's resolved shim entry for cmd (set at
// session start), used to answer resolve_shim IPC requests.
func lookupActiveShim(cmd string) (resolvedShim, bool) {
	activeShimMu.RLock()
	defer activeShimMu.RUnlock()
	rs, ok := activeShimReg[cmd]
	return rs, ok
}

// resolveRealBinary finds cmd on PATH, skipping any greenlight shim so the shim
// reaches the genuine OS binary (e.g. the real `gh`) instead of looping back
// into greenlight. Two filters, because either alone is insufficient:
//
//   - Any PATH entry living in a per-session shim dir (base name
//     "greenlight-bin-<relayID>"). This catches a *foreign* greenlight shim —
//     one pointing at a different greenlight build — which the self check below
//     cannot, since it resolves to some other binary, not us. Two such shims on
//     PATH (e.g. a greenlight session nested inside another, or a stale shim
//     dir) would otherwise exec each other forever (issue #131).
//   - Any candidate resolving to the running binary itself, kept as a second
//     guard for an own shim that somehow sits outside a greenlight-bin- dir.
func resolveRealBinary(cmd string) (string, error) {
	self, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = resolved
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		// Skip every entry in a greenlight shim dir, whichever build it points at.
		if strings.HasPrefix(filepath.Base(dir), shimDirPrefix) {
			continue
		}
		cand := filepath.Join(dir, cmd)
		fi, statErr := os.Stat(cand)
		if statErr != nil || fi.IsDir() || fi.Mode()&0111 == 0 {
			continue
		}
		// Skip our own shim symlink (resolves to the greenlight binary).
		if resolved, rerr := filepath.EvalSymlinks(cand); rerr == nil && resolved == self {
			continue
		}
		return cand, nil
	}
	return "", fmt.Errorf("not found: %s", cmd)
}

// execReal replaces the current process with the real binary, inheriting the
// full environment and stdio. Used when no secret is available so the command
// behaves exactly as if greenlight weren't shimming it.
func execReal(real string, args []string) {
	argv := append([]string{real}, args...)
	if err := syscall.Exec(real, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: exec %s: %v\n", real, err)
		os.Exit(126)
	}
}
