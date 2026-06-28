//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsWorktreeMutationCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"git worktree add ../repo-feature", true},
		{"git worktree add -b feat ../wt feat", true},
		{"git worktree remove ../wt", true},
		{"git worktree move ../wt ../wt2", true},
		{"git worktree list", false},
		{"git worktree prune", false},
		{"git status", false},
		{"git commit -m worktree", false},
		{"echo git worktree add", false}, // not the leading command
	}
	for _, tt := range tests {
		if got := isWorktreeMutationCommand(tt.cmd); got != tt.want {
			t.Errorf("isWorktreeMutationCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestParseWorktreeAddPath(t *testing.T) {
	cwd := "/Users/x/repo"
	tests := []struct {
		name string
		cmd  string
		want string // expected suffix (canonicalization may resolve symlinks); "" means empty
	}{
		{"absolute path", "git worktree add /tmp/wt-a", "/tmp/wt-a"},
		{"relative path joined to cwd", "git worktree add ../repo-feature", "/Users/x/repo-feature"},
		{"with -b branch flag", "git worktree add -b feature /tmp/wt-b feature", "/tmp/wt-b"},
		{"with --reason value flag", "git worktree add --reason because /tmp/wt-c", "/tmp/wt-c"},
		{"force flag then path", "git worktree add -f /tmp/wt-d", "/tmp/wt-d"},
		{"glob bails", "git worktree add /tmp/wt-*", ""},
		{"not an add", "git worktree remove /tmp/wt", ""},
		// The exact form from issue #148: relative dest before -b, trailing 2>&1.
		{"reported form: dest before -b with redirect",
			"git worktree add ../permit-ticket-145-autopilot-prompt -b ticket-145-autopilot-prompt 2>&1",
			"/Users/x/permit-ticket-145-autopilot-prompt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWorktreeAddPath(tt.cmd, cwd)
			if tt.want == "" {
				if got != "" {
					t.Errorf("parseWorktreeAddPath(%q) = %q, want empty", tt.cmd, got)
				}
				return
			}
			// canonicalizePath cleans but a nonexistent path won't resolve
			// symlinks, so it should equal the lexical clean.
			if got != filepath.Clean(tt.want) {
				t.Errorf("parseWorktreeAddPath(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestIsEphemeralRoot(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/tmp", true},
		{"/tmp/foo", true},
		{"/private/tmp", true},
		{"/private/tmp/claude-501/x", true},
		{"/var/folders/ab/cd/T", true},
		{"/run/user/1000", true},
		{"/", false},
		{"", false},
		{"/Users/x/repo", false},
		{"/home/user/project", false},
		{"/tmpfoo", false}, // not under /tmp/
	}
	for _, tt := range tests {
		if got := isEphemeralRoot(tt.path); got != tt.want {
			t.Errorf("isEphemeralRoot(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestScratchRoots_ValidatesTMPDIR(t *testing.T) {
	// A TMPDIR pointing at a non-ephemeral location must be skipped.
	t.Setenv("TMPDIR", "/Users/x/not-temp")
	t.Setenv("TMP", "")
	roots := scratchRoots("/Users/x/repo")
	for _, r := range roots {
		if r.Path == "/Users/x/not-temp" {
			t.Errorf("non-ephemeral TMPDIR leaked into scratch roots: %v", roots)
		}
		if r.Kind != cliRootKindScratch {
			t.Errorf("scratch root has wrong kind: %v", r)
		}
	}
	// /tmp is always a candidate and is ephemeral, so it should be present.
	found := false
	for _, r := range roots {
		if r.Path == "/tmp" || r.Path == "/private/tmp" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected /tmp among scratch roots, got %v", roots)
	}
}

func TestScratchRoots_SkipsRootContainingProject(t *testing.T) {
	// A project living under a real temp dir must not have the whole temp tree
	// reported as ephemeral.
	tmp := t.TempDir() // typically under /tmp or /var/folders (ephemeral)
	if !isEphemeralRoot(canonicalizePath(tmp)) {
		t.Skipf("test tempdir %q is not under an ephemeral prefix", tmp)
	}
	proj := filepath.Join(tmp, "myproject")
	os.MkdirAll(proj, 0755)
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", "")
	roots := scratchRoots(proj)
	canonTmp := canonicalizePath(tmp)
	for _, r := range roots {
		if r.Path == canonTmp {
			t.Errorf("scratch root %q contains the project %q — should be skipped", r.Path, proj)
		}
	}
}

func TestSnapshotIncludesScratchAndWorktrees(t *testing.T) {
	m := &sessionRootsManager{
		worktrees: []SessionRoot{{Path: "/Users/x/repo", Kind: cliRootKindWorktree}},
		scratch:   []SessionRoot{{Path: "/private/tmp", Kind: cliRootKindScratch}},
	}
	snap := m.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
}

func TestRootContains(t *testing.T) {
	tests := []struct {
		root, p string
		want    bool
	}{
		{"/Users/x/repo", "/Users/x/repo", true},
		{"/Users/x/repo", "/Users/x/repo/main.go", true},
		// The #148 sibling case: a cwd prefix must not swallow a sibling worktree.
		{"/Users/x/permit", "/Users/x/permit-ticket-145/main.go", false},
		{"/Users/x/permit", "/Users/x/permit/main.go", true},
		{"", "/Users/x/a", false},
		{"/Users/x/a", "", false},
	}
	for _, tt := range tests {
		if got := rootContains(tt.root, tt.p); got != tt.want {
			t.Errorf("rootContains(%q, %q) = %v, want %v", tt.root, tt.p, got, tt.want)
		}
	}
}

// newTestRootsManager builds a manager with a stub enumerate func and no daemon,
// so reconcile logic can be exercised deterministically without git or a WS.
func newTestRootsManager(cwd string, enumerate func(string) []string) *sessionRootsManager {
	return &sessionRootsManager{
		cwd:       cwd,
		canonCwd:  cwd,
		negCache:  map[string]time.Time{},
		enumerate: enumerate,
	}
}

func TestReconcileForPath_AlreadyTrackedSkipsEnumeration(t *testing.T) {
	calls := 0
	m := newTestRootsManager("/Users/x/repo", func(string) []string {
		calls++
		return nil
	})
	// A path inside cwd is already trusted — no enumeration, returns true.
	if !m.reconcileForPath("/Users/x/repo/main.go") {
		t.Errorf("reconcileForPath(in-cwd) = false, want true")
	}
	if calls != 0 {
		t.Errorf("enumerate called %d times for an in-cwd path, want 0", calls)
	}
}

func TestReconcileForPath_DiscoversSiblingWorktree(t *testing.T) {
	wt := "/Users/x/permit-ticket-145"
	m := newTestRootsManager("/Users/x/permit", func(string) []string {
		// Enumeration now reports the sibling worktree (it exists by edit time).
		return []string{"/Users/x/permit", wt}
	})
	if !m.reconcileForPath(wt + "/main.go") {
		t.Fatalf("reconcileForPath(in new worktree) = false, want true")
	}
	found := false
	for _, r := range m.worktrees {
		if r.Path == wt {
			found = true
		}
	}
	if !found {
		t.Errorf("worktree %q not added to manager: %v", wt, m.worktrees)
	}
}

func TestReconcileForPath_NegativeCacheSuppressesReEnumeration(t *testing.T) {
	calls := 0
	m := newTestRootsManager("/Users/x/repo", func(string) []string {
		calls++
		return []string{"/Users/x/repo"} // never contains the outside path
	})
	outside := "/Users/x/somewhere-else/main.go"
	if m.reconcileForPath(outside) {
		t.Errorf("reconcileForPath(outside) = true, want false")
	}
	if m.reconcileForPath(outside) {
		t.Errorf("reconcileForPath(outside) second call = true, want false")
	}
	if calls != 1 {
		t.Errorf("enumerate called %d times, want 1 (negative cache should suppress the 2nd)", calls)
	}
	// A worktree change clears the negative cache, so the path is re-checked.
	m.onWorktreeChange("git worktree add /tmp/unrelated")
	if m.reconcileForPath(outside) {
		t.Errorf("reconcileForPath(outside) after cache clear = true, want false")
	}
	if calls < 2 {
		t.Errorf("enumerate not re-run after negative-cache clear: calls=%d", calls)
	}
}

func TestReconcileForPath_RelativePathIgnored(t *testing.T) {
	calls := 0
	m := newTestRootsManager("/Users/x/repo", func(string) []string {
		calls++
		return nil
	})
	// A non-absolute operand resolves against cwd server-side; never reconcile.
	if m.reconcileForPath("relative/path.go") {
		t.Errorf("reconcileForPath(relative) = true, want false")
	}
	if calls != 0 {
		t.Errorf("enumerate called %d times for a relative path, want 0", calls)
	}
}

// TestReconcileForPath_RealGitWorktree exercises the durable path end-to-end:
// a worktree added mid-session (after the manager's baseline enumeration) is
// discovered by reconcileForPath via a real `git worktree list`.
func TestReconcileForPath_RealGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	parent := t.TempDir()
	repo := filepath.Join(parent, "permit")
	mustGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(repo, 0755)
	mustGit(repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0644)
	mustGit(repo, "add", ".")
	mustGit(repo, "commit", "-qm", "init")

	// Manager baseline: only the main worktree is known.
	m := newSessionRootsManager(nil, "relay-1", repo, false)
	wt := filepath.Join(parent, "permit-ticket-148")
	editPath := filepath.Join(wt, "edit.go")

	// Before the worktree exists, an edit there is outside every root.
	if m.reconcileForPath(editPath) {
		t.Errorf("path reconciled before worktree exists")
	}

	// Add the worktree for real, then create a file so EvalSymlinks resolves.
	mustGit(repo, "worktree", "add", "-q", wt, "-b", "ticket-148")
	os.WriteFile(editPath, []byte("y"), 0644)

	// The negative cache from the pre-existence check would suppress discovery;
	// in production a worktree add fires trigger 2 which clears it. Simulate that
	// the cache window has passed by clearing it directly.
	m.mu.Lock()
	m.negCache = map[string]time.Time{}
	m.mu.Unlock()

	if !m.reconcileForPath(editPath) {
		t.Fatalf("reconcileForPath did not discover real worktree %q; roots=%v", wt, m.worktrees)
	}
	canonWT := canonicalizePath(wt)
	found := false
	for _, r := range m.worktrees {
		if r.Path == canonWT {
			found = true
		}
	}
	if !found {
		t.Errorf("canonical worktree %q missing from roots %v", canonWT, m.worktrees)
	}
}

func TestClaudeScratchpadDir(t *testing.T) {
	const slug = "-Users-davidfarrell-permit"
	const uuid = "b65648d2-1234-4abc-9def-0123456789ab"
	transcript := filepath.Join(os.Getenv("HOME"), ".claude", "projects", slug, uuid+".jsonl")

	uidTag := fmt.Sprintf("claude-%d", os.Getuid())
	wantSuffix := "/" + slug + "/" + uuid + "/scratchpad"

	for _, agent := range []string{"claude", "claude-code"} {
		got := claudeScratchpadDir(agent, transcript)
		if got == "" {
			t.Fatalf("claudeScratchpadDir(%q) = empty, want a path", agent)
		}
		if !strings.HasSuffix(got, wantSuffix) {
			t.Errorf("claudeScratchpadDir(%q) = %q, want suffix %q (slug+uuid embedded)", agent, got, wantSuffix)
		}
		if !strings.Contains(got, uidTag) {
			t.Errorf("claudeScratchpadDir(%q) = %q, want it to contain %q", agent, got, uidTag)
		}
		// macOS canonicalizes /tmp → /private/tmp; the result must never be the
		// literal /tmp form when a /private/tmp form is real (covered more
		// directly by TestCanonicalizeExistingPrefix).
		if !filepath.IsAbs(got) {
			t.Errorf("claudeScratchpadDir(%q) = %q, not absolute", agent, got)
		}
	}

	// Non-Claude agents have no harness scratchpad.
	for _, agent := range []string{"codex", "copilot", "cursor", "gemini", "pi", "", "unknown"} {
		if got := claudeScratchpadDir(agent, transcript); got != "" {
			t.Errorf("claudeScratchpadDir(%q) = %q, want empty", agent, got)
		}
	}

	// No transcript path → no scratchpad.
	if got := claudeScratchpadDir("claude", ""); got != "" {
		t.Errorf("claudeScratchpadDir(claude, \"\") = %q, want empty", got)
	}
}

func TestCanonicalizeExistingPrefix(t *testing.T) {
	tmp := t.TempDir()
	realBase, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(realBase, "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realBase, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}

	// Leaf absent: the existing symlink prefix resolves, the missing remainder
	// is preserved. This is the scratchpad case (/tmp → /private/tmp, leaf dir
	// not yet created).
	got := canonicalizeExistingPrefix(filepath.Join(link, "a", "b", "leaf"))
	want := filepath.Join(realDir, "a", "b", "leaf")
	if got != want {
		t.Errorf("canonicalizeExistingPrefix(symlink + missing leaf) = %q, want %q", got, want)
	}

	// Fully existing symlink resolves entirely.
	if got := canonicalizeExistingPrefix(link); got != realDir {
		t.Errorf("canonicalizeExistingPrefix(existing symlink) = %q, want %q", got, realDir)
	}

	// Non-absolute path is rejected.
	if got := canonicalizeExistingPrefix("relative/path"); got != "" {
		t.Errorf("canonicalizeExistingPrefix(relative) = %q, want empty", got)
	}
	if got := canonicalizeExistingPrefix(""); got != "" {
		t.Errorf("canonicalizeExistingPrefix(\"\") = %q, want empty", got)
	}
}

func scratchPaths(m *sessionRootsManager) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.scratch {
		out = append(out, r.Path)
	}
	return out
}

func TestAddScratchpadRoot(t *testing.T) {
	tmp := t.TempDir() // under /var/folders (macOS) or /tmp (Linux) — both ephemeral
	canonTmp := canonicalizeExistingPrefix(tmp)
	sp := filepath.Join(tmp, "claude-x", "slug", "uuid", "scratchpad")
	canonSp := canonicalizeExistingPrefix(sp)

	// nil receiver is safe.
	var mnil *sessionRootsManager
	mnil.addScratchpadRoot(sp)

	m := newSessionRootsManager(nil, "relay-1", "/some/project", false)

	// Adds once.
	m.addScratchpadRoot(sp)
	if got := scratchPaths(m); len(got) != 1 || got[0] != canonSp {
		t.Fatalf("after add, scratch = %v, want [%s]", got, canonSp)
	}

	// Idempotent.
	m.addScratchpadRoot(sp)
	if got := scratchPaths(m); len(got) != 1 {
		t.Fatalf("after re-add, scratch = %v, want length 1", got)
	}

	// Non-ephemeral and empty paths are rejected.
	m.addScratchpadRoot("/etc/definitely-not-ephemeral/scratchpad")
	m.addScratchpadRoot("")
	if got := scratchPaths(m); len(got) != 1 {
		t.Fatalf("rejected paths changed scratch: %v", got)
	}

	// A scratchpad already covered by the blanket scratch root (scratch_auto=on
	// case) is not added as a duplicate.
	m2 := newSessionRootsManager(nil, "relay-2", "/some/project", false)
	m2.scratch = []SessionRoot{{Path: canonTmp, Kind: cliRootKindScratch}}
	m2.addScratchpadRoot(sp) // canonSp sits under canonTmp
	if got := scratchPaths(m2); len(got) != 1 || got[0] != canonTmp {
		t.Fatalf("scratchpad under blanket root should not be added; scratch = %v", got)
	}
}
