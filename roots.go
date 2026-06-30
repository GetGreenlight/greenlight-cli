//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionRoot is a directory the CLI vouches for, reported to the server so it
// can auto-approve file operations inside it (issue #119). The CLI is the trust
// anchor: only it can enumerate git worktrees or read $TMPDIR, so it derives the
// set and the server matches against it.
//
//	kind == "worktree" → scoped    (shares the project's .git; same trust as cwd)
//	kind == "scratch"  → ephemeral (OS temp dir; destructive ops also auto)
type SessionRoot struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

const (
	cliRootKindWorktree = "worktree"
	cliRootKindScratch  = "scratch"
)

// ephemeralRootPrefixes are the canonical locations under which a directory is
// accepted as ephemeral scratch space. A resolved $TMPDIR/$TMP that does not sit
// under one of these is skipped — never trust an arbitrary user-set TMPDIR.
var ephemeralRootPrefixes = []string{
	"/tmp",
	"/private/tmp", // macOS: /tmp is a symlink here
	"/var/folders", // macOS: per-user $TMPDIR (/var/folders/ab/.../T)
	"/private/var/folders",
	"/run/user", // Linux: XDG runtime dir
}

// enumerateRoots assembles the trusted-root set for a session: every git
// worktree of the project (scoped) plus the OS scratch dirs (ephemeral, gated by
// scratchAuto). cwd is the session working directory; it doubles as an implicit
// scoped root server-side, so it need not appear here, but the main worktree
// (which usually equals cwd's repo root) is included naturally by enumeration.
func enumerateRoots(cwd string, scratchAuto bool) []SessionRoot {
	var roots []SessionRoot
	for _, p := range worktreeRoots(cwd) {
		roots = append(roots, SessionRoot{Path: p, Kind: cliRootKindWorktree})
	}
	if scratchAuto {
		roots = append(roots, scratchRoots(cwd)...)
	}
	return roots
}

// worktreeRoots returns the canonical paths of every git worktree linked to the
// repo containing cwd, via `git worktree list --porcelain`. Returns nil when cwd
// isn't a git repo or git is unavailable — a non-git project simply has no
// worktree roots.
func worktreeRoots(cwd string) []string {
	if cwd == "" {
		return nil
	}
	cmd := exec.Command("git", "-C", cwd, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		if p == "" {
			continue
		}
		if canon := canonicalizePath(p); canon != "" && !seen[canon] {
			seen[canon] = true
			paths = append(paths, canon)
		}
	}
	return paths
}

// scratchRoots resolves $TMPDIR, $TMP and /tmp to ephemeral roots, validated and
// de-duplicated. Each candidate is reported in BOTH its symlink-resolved form
// (e.g. /private/tmp on macOS, today's behavior) and its lexically-cleaned,
// non-resolved form (e.g. /tmp) — because the server matches operands lexically
// and cannot resolve the user's symlinks, so an agent that writes a bare /tmp
// path only matches if /tmp itself is a reported root (issue #208). On Linux the
// two forms coincide and the seen-dedup collapses them to one entry. A form is
// included only if it sits under a known ephemeral prefix and does not contain
// (or equal) the project cwd, so a project living under /tmp can't have its whole
// tree become ephemeral.
func scratchRoots(cwd string) []SessionRoot {
	canonCwd := canonicalizePath(cwd) // resolved space
	lexCwd := lexicalAbs(cwd)         // literal space
	var roots []SessionRoot
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] || !isEphemeralRoot(p) {
			return
		}
		// Never let a scratch root contain (or equal) the project tree. p may be
		// a literal form while the cwd is only known resolved (or vice versa), so
		// check against the cwd in BOTH normalization spaces — a single-space
		// prefix check silently fails to exclude the cross-space form.
		for _, c := range []string{canonCwd, lexCwd} {
			if c != "" && (p == c || strings.HasPrefix(c, p+"/") || strings.HasPrefix(p, c+"/")) {
				return
			}
		}
		seen[p] = true
		roots = append(roots, SessionRoot{Path: p, Kind: cliRootKindScratch})
	}
	for _, cand := range []string{os.Getenv("TMPDIR"), os.Getenv("TMP"), "/tmp"} {
		if cand == "" {
			continue
		}
		add(canonicalizePath(cand)) // resolved: /private/tmp
		add(lexicalAbs(cand))       // literal:  /tmp  (no-op dup on Linux)
	}
	return roots
}

// lexicalAbs cleans an absolute path without resolving symlinks, returning "" for
// a non-absolute (or empty) path. The counterpart to canonicalizePath for cases
// where the literal, un-resolved form of a path must also be reported (#208).
func lexicalAbs(p string) string {
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

// isEphemeralRoot reports whether a canonical path sits under a known ephemeral
// location. Refuses "/" and the bare prefixes themselves are allowed (they ARE
// the scratch root), but a path must be exactly a prefix or under one.
func isEphemeralRoot(canon string) bool {
	if canon == "" || canon == "/" {
		return false
	}
	for _, p := range ephemeralRootPrefixes {
		if canon == p || strings.HasPrefix(canon, p+"/") {
			return true
		}
	}
	return false
}

// canonicalizePath resolves symlinks and returns a cleaned absolute path. Falls
// back to a lexical clean if the path can't be stat'd (e.g. a worktree that was
// optimistically registered before `git worktree add` ran). Returns "" for a
// non-absolute path.
func canonicalizePath(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

// canonicalizeExistingPrefix resolves symlinks in the longest existing ancestor
// of p and re-joins the non-existent remainder, returning a cleaned absolute
// path. Unlike canonicalizePath (which falls back to the *literal* path the
// moment EvalSymlinks fails), this keeps the symlink-resolved prefix even when
// the leaf doesn't exist yet — essential for the scratchpad root, whose leaf dir
// is usually absent at report time but whose parent (/tmp → /private/tmp on
// macOS) is a symlink the server's write operands resolve through. Returns "" for
// a non-absolute path.
func canonicalizeExistingPrefix(p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	// Walk up until an ancestor exists (the filesystem root always does),
	// collecting the trailing components we stripped.
	existing := p
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break // reached root without a stat hit; resolve nothing
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	if resolved, err := filepath.EvalSymlinks(existing); err == nil {
		existing = resolved
	}
	return filepath.Clean(filepath.Join(append([]string{existing}, rest...)...))
}

// claudeScratchpadDir returns the Claude Code session scratchpad directory for
// the running session, or "" for any agent without such a harness scratchpad.
// Claude Code reuses one session UUID for both the transcript filename and the
// scratchpad dir, so the path is derivable from the resolved transcript path with
// no need to replicate Claude's project-slug transform (issue #182):
//
//	transcript: ~/.claude/projects/<slug>/<uuid>.jsonl
//	scratchpad: /tmp/claude-<uid>/<slug>/<uuid>/scratchpad
//
// The result is canonicalized to its real on-disk form (e.g. /private/tmp on
// macOS) via canonicalizeExistingPrefix so it matches the server's canonicalized
// write operands even when the leaf dir doesn't exist yet.
func claudeScratchpadDir(agent, transcriptPath string) string {
	if agent != "claude" && agent != "claude-code" {
		return ""
	}
	if transcriptPath == "" {
		return ""
	}
	base := filepath.Base(transcriptPath)
	uuid := strings.TrimSuffix(base, filepath.Ext(base))
	if uuid == "" || uuid == "." {
		return ""
	}
	slug := filepath.Base(filepath.Dir(transcriptPath))
	if slug == "" || slug == "." || slug == string(filepath.Separator) {
		return ""
	}
	raw := fmt.Sprintf("/tmp/claude-%d/%s/%s/scratchpad", os.Getuid(), slug, uuid)
	return canonicalizeExistingPrefix(raw)
}

// rootsNegCacheTTL bounds how long a "this path is outside every root" verdict
// suppresses re-enumeration in the trigger-3 lazy reconcile, so an agent that
// repeatedly touches a genuinely-external path doesn't spawn a `git` subprocess
// on every op. Short, because the only thing it can miss is a worktree added in
// the window — and trigger 2 plus the next miss after expiry both recover it.
const rootsNegCacheTTL = 3 * time.Second

// sessionRootsManager owns a session's reported root set and pushes updates to
// the server. Scratch roots are static for the session; worktree roots are
// dynamic (an agent may `git worktree add` mid-session), so they're recomputed
// and re-sent on the triggers in issue #119 §4.1.
type sessionRootsManager struct {
	mu          sync.Mutex
	daemon      *DaemonWS
	relayID     string
	cwd         string
	canonCwd    string // cwd, symlink-resolved, for path matching
	scratchAuto bool
	scratch     []SessionRoot // static for the session
	worktrees   []SessionRoot // dynamic, by canonical path
	// negCache records absolute paths recently found outside every root, with
	// the time of the check (trigger 3). Cleared whenever the worktree set
	// changes so a freshly-added worktree isn't shadowed by a stale verdict.
	negCache map[string]time.Time
	// enumerate returns the repo's current worktree paths; a field so tests can
	// substitute a stub. Defaults to worktreeRoots.
	enumerate func(cwd string) []string
}

// newSessionRootsManager builds the manager and computes the baseline root set.
func newSessionRootsManager(daemon *DaemonWS, relayID, cwd string, scratchAuto bool) *sessionRootsManager {
	m := &sessionRootsManager{
		daemon:      daemon,
		relayID:     relayID,
		cwd:         cwd,
		canonCwd:    canonicalizePath(cwd),
		scratchAuto: scratchAuto,
		negCache:    map[string]time.Time{},
		enumerate:   worktreeRoots,
	}
	if scratchAuto {
		m.scratch = scratchRoots(cwd)
	}
	for _, p := range worktreeRoots(cwd) {
		m.worktrees = append(m.worktrees, SessionRoot{Path: p, Kind: cliRootKindWorktree})
	}
	return m
}

// addScratchpadRoot registers the session's Claude scratchpad as a kind:"scratch"
// trusted root and pushes the full set to the server (issue #182). Reported
// independently of scratch_auto: when scratch_auto is on the blanket /private/tmp
// root already covers it (pathTrackedLocked dedupes, so no duplicate push); when
// off, this narrow per-session root is the only thing trusting the scratchpad —
// the case the ticket is about. Idempotent and nil-safe; a path that doesn't
// canonicalize under a known ephemeral prefix is rejected.
func (m *sessionRootsManager) addScratchpadRoot(path string) {
	if m == nil || path == "" {
		return
	}
	canon := canonicalizeExistingPrefix(path)
	if canon == "" || !isEphemeralRoot(canon) {
		return
	}
	m.mu.Lock()
	// Already covered by cwd / a worktree / the blanket scratch root, or already
	// added — nothing to push.
	if m.pathTrackedLocked(canon) {
		m.mu.Unlock()
		return
	}
	m.scratch = append(m.scratch, SessionRoot{Path: canon, Kind: cliRootKindScratch})
	roots := m.snapshotLocked()
	m.mu.Unlock()

	if m.daemon != nil {
		m.daemon.SendSessionRoots(m.relayID, roots)
	}
	log.Printf("roots: added session scratchpad root %q", canon)
}

// snapshot returns the current full root set (worktrees + scratch).
func (m *sessionRootsManager) snapshot() []SessionRoot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked returns the full root set; the caller must hold m.mu.
func (m *sessionRootsManager) snapshotLocked() []SessionRoot {
	out := make([]SessionRoot, 0, len(m.worktrees)+len(m.scratch))
	out = append(out, m.worktrees...)
	out = append(out, m.scratch...)
	return out
}

// rootContains reports whether canonical absolute path p is root itself or
// strictly under it. Mirrors the server's pathInside so the CLI's local
// trigger-3 decision agrees with what the server will match.
func rootContains(root, p string) bool {
	if root == "" || p == "" {
		return false
	}
	if p == root {
		return true
	}
	sep := root
	if !strings.HasSuffix(sep, "/") {
		sep += "/"
	}
	return strings.HasPrefix(p, sep)
}

// pathTrackedLocked reports whether canonical path p is inside the cwd or any
// currently tracked root (worktree or scratch). The caller must hold m.mu.
func (m *sessionRootsManager) pathTrackedLocked(p string) bool {
	if rootContains(m.canonCwd, p) {
		return true
	}
	for _, r := range m.worktrees {
		if rootContains(r.Path, p) {
			return true
		}
	}
	for _, r := range m.scratch {
		if rootContains(r.Path, p) {
			return true
		}
	}
	return false
}

// reconcileForPath is issue #119 trigger 3 (added in #148): the durable safety
// net for a worktree created mid-session that trigger 2 missed. Called with a
// file op's target path before the permission request is forwarded; when the
// path is outside every tracked root, it re-enumerates the repo's worktrees
// (the worktree provably exists by now, so `git worktree list` reports it
// authoritatively — no argv guessing, no relative-cwd ambiguity) and, if the
// set changed, updates the manager and pushes session_roots SYNCHRONOUSLY so
// the server has the new root before the request arrives. Returns true when the
// path is now covered. Conservative by construction: it only ever adds
// git-derived worktrees of the session's repo, never an agent-made directory.
func (m *sessionRootsManager) reconcileForPath(rawPath string) bool {
	p := canonicalizePath(rawPath)
	if p == "" {
		return false // non-absolute / unresolvable — let the server decide
	}

	m.mu.Lock()
	if m.pathTrackedLocked(p) {
		m.mu.Unlock()
		return true // already trusted; nothing to do
	}
	if t, ok := m.negCache[p]; ok && time.Since(t) < rootsNegCacheTTL {
		m.mu.Unlock()
		return false // recently confirmed outside; don't re-enumerate
	}
	enumerate := m.enumerate
	cwd := m.cwd
	m.mu.Unlock()

	// Re-enumerate outside the lock (runs a git subprocess).
	var fresh []SessionRoot
	for _, wp := range enumerate(cwd) {
		fresh = append(fresh, SessionRoot{Path: wp, Kind: cliRootKindWorktree})
	}

	m.mu.Lock()
	changed := !sameRootPaths(m.worktrees, fresh)
	if changed {
		m.worktrees = fresh
		m.negCache = map[string]time.Time{} // worktrees changed: drop stale verdicts
	}
	tracked := m.pathTrackedLocked(p)
	if !tracked {
		m.negCache[p] = time.Now()
	}
	roots := m.snapshotLocked()
	m.mu.Unlock()

	if changed && m.daemon != nil {
		m.daemon.SendSessionRoots(m.relayID, roots)
		log.Printf("roots: trigger-3 reconcile discovered %d worktree root(s) for %q", len(fresh), p)
	}
	return tracked
}

// sameRootPaths reports whether two root slices carry the same set of paths
// (order-independent), used to decide whether a re-enumeration actually changed
// the worktree set.
func sameRootPaths(a, b []SessionRoot) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, r := range a {
		set[r.Path] = true
	}
	for _, r := range b {
		if !set[r.Path] {
			return false
		}
	}
	return true
}

// onWorktreeChange reconciles worktree roots after an approved `git worktree
// add/remove/move` and pushes the full set to the server. Gating happens before
// exec, so a freshly-added worktree isn't visible to `git worktree list` yet —
// the destination path is parsed from argv and registered optimistically so it's
// trusted before the agent's first edit can land there. Stale/removed entries
// self-heal because the set is replaced wholesale on each send.
func (m *sessionRootsManager) onWorktreeChange(cmd string) {
	live := m.enumerate(m.cwd) // current entries (won't include a pending add)
	set := map[string]bool{}
	var wt []SessionRoot
	add := func(p string) {
		if p != "" && !set[p] {
			set[p] = true
			wt = append(wt, SessionRoot{Path: p, Kind: cliRootKindWorktree})
		}
	}
	for _, p := range live {
		add(p)
	}
	if p := parseWorktreeAddPath(cmd, m.cwd); p != "" {
		add(p) // optimistic: registered before the command finishes
	}

	m.mu.Lock()
	m.worktrees = wt
	m.negCache = map[string]time.Time{} // worktrees changed: drop stale "outside" verdicts
	roots := m.snapshotLocked()
	m.mu.Unlock()

	if m.daemon != nil {
		m.daemon.SendSessionRoots(m.relayID, roots)
	}
	log.Printf("roots: reconciled %d worktree root(s) after %q", len(wt), cmd)
}

// isWorktreeMutationCommand reports whether any segment of a Bash command is a
// `git worktree add/remove/move` — the events that change the worktree set. Only
// a segment whose leading command is `git` counts, so `echo git worktree add`
// (or a path argument that happens to contain those words) doesn't trigger.
func isWorktreeMutationCommand(cmd string) bool {
	for _, seg := range splitCompoundCommand(cmd) {
		if worktreeMutationVerb(seg) != "" {
			return true
		}
	}
	return false
}

// worktreeMutationVerb returns "add"/"remove"/"move" if the segment's leading
// command is a matching `git worktree` invocation, else "". Leading `VAR=val`
// assignments are skipped.
func worktreeMutationVerb(seg string) string {
	fields := strings.Fields(seg)
	i := 0
	for i < len(fields) && strings.Contains(fields[i], "=") && !strings.HasPrefix(fields[i], "-") {
		i++ // skip env assignments
	}
	if i+2 >= len(fields) {
		return ""
	}
	if fields[i] != "git" || fields[i+1] != "worktree" {
		return ""
	}
	switch fields[i+2] {
	case "add", "remove", "move":
		return fields[i+2]
	}
	return ""
}

// worktreeAddValueFlags are `git worktree add` options that consume the next
// token as their value, so the destination-path scan must skip that token too.
var worktreeAddValueFlags = map[string]bool{
	"-b": true, "-B": true, "--reason": true,
}

// parseWorktreeAddPath extracts the destination path from a `git worktree add`
// command, resolved against cwd and canonicalized. Returns "" when the command
// isn't an add or the path can't be determined.
func parseWorktreeAddPath(cmd, cwd string) string {
	for _, seg := range splitCompoundCommand(cmd) {
		if worktreeMutationVerb(seg) == "add" {
			return parseWorktreeAddPathSegment(seg, cwd)
		}
	}
	return ""
}

func parseWorktreeAddPathSegment(seg, cwd string) string {
	fields := strings.Fields(seg)
	// Locate "git worktree add".
	start := -1
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] == "git" && fields[i+1] == "worktree" && fields[i+2] == "add" {
			start = i + 3
			break
		}
	}
	if start < 0 {
		return ""
	}
	for i := start; i < len(fields); i++ {
		f := fields[i]
		if worktreeAddValueFlags[f] {
			i++ // skip the flag's value
			continue
		}
		if strings.HasPrefix(f, "-") {
			continue // boolean flag
		}
		// Bail on anything we can't resolve cleanly.
		if strings.ContainsAny(f, "*?[]${}()`~\"'\\") {
			return ""
		}
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		return canonicalizePath(p)
	}
	return ""
}
