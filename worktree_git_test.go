//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a fresh git repo with an initial commit on `main`.
// Returns the repo dir.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func TestCommitCount(t *testing.T) {
	dir := initRepo(t)
	// Tag a base so we can rev-list base..HEAD without a remote.
	runGit(t, dir, "tag", "base")

	n, err := commitCount(dir, "base", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 commits ahead, got %d", n)
	}

	// Make a commit on top of base.
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f")
	runGit(t, dir, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "-m", "c1")

	n, err = commitCount(dir, "base", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 commit ahead, got %d", n)
	}
}

func TestWorkingTreeDirty(t *testing.T) {
	dir := initRepo(t)
	dirty, err := workingTreeDirty(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Errorf("fresh repo should be clean")
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	dirty, err = workingTreeDirty(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Errorf("untracked file should mark tree dirty")
	}
}

func TestBranchForWorktree(t *testing.T) {
	dir := initRepo(t)
	if got := branchForWorktree(dir); got != "main" {
		t.Errorf("branchForWorktree = %q, want main", got)
	}
	runGit(t, dir, "checkout", "-q", "-b", "gl/42-foo")
	if got := branchForWorktree(dir); got != "gl/42-foo" {
		t.Errorf("branchForWorktree = %q, want gl/42-foo", got)
	}
}

// defaultBranchFromRemote reads refs/remotes/origin/HEAD. Without a real
// remote we set up a local bare repo and clone it, which gives us the
// symbolic ref pointing at the default branch.
func TestDefaultBranchFromRemote(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "-q", "--bare", "-b", "main")

	src := initRepo(t)
	runGit(t, src, "remote", "add", "origin", bare)
	runGit(t, src, "push", "-q", "-u", "origin", "main")

	// Clone freshly so origin/HEAD is populated.
	clone := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", bare, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	got := defaultBranchFromRemote(clone)
	if got != "main" {
		t.Errorf("defaultBranchFromRemote = %q, want main", got)
	}
}

// prepareTicketWorktree should be a no-op when origin doesn't match the
// ticket's owner/repo — it logs and returns the original cwd.
func TestPrepareTicketWorktree_OriginMismatch(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/other/repo.git")

	got := prepareTicketWorktree("github:foo/bar#1", dir, nil)
	if got != dir {
		t.Errorf("got %q, want original cwd %q", got, dir)
	}
}

func TestPrepareTicketWorktree_NotARepo(t *testing.T) {
	dir := t.TempDir() // not a git repo
	got := prepareTicketWorktree("github:foo/bar#1", dir, nil)
	if got != dir {
		t.Errorf("non-repo cwd: got %q, want %q", got, dir)
	}
}

// Happy path + idempotent reuse. Uses a local bare repo as origin so the
// `git worktree add` command can resolve `origin/<default>`. We override
// HOME so the worktree lands under a tempdir.
func TestPrepareTicketWorktree_HappyAndIdempotent(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "-q", "--bare", "-b", "main")

	// Use a clone so origin/HEAD is populated (defaultBranchFromRemote
	// reads refs/remotes/origin/HEAD).
	upstream := initRepo(t)
	runGit(t, upstream, "remote", "add", "origin", bare)
	runGit(t, upstream, "push", "-q", "-u", "origin", "main")

	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	// Re-point origin to a github.com URL so repoFromCwd parses owner/repo
	// correctly. The actual fetch is against the bare repo via the previous
	// clone state — we don't push/fetch after this.
	runGit(t, clone, "remote", "set-url", "origin", "https://github.com/foo/bar.git")
	// But the worktree-add needs a real refspec resolvable from the local
	// objects: origin/main is still present from the clone.

	home := t.TempDir()
	t.Setenv("HOME", home)

	gotCwd := prepareTicketWorktree("github:foo/bar#42", clone, func(_, _ string, _ int) string {
		return "Add the thing"
	})
	wantPath := filepath.Join(home, ".greenlight", "worktrees", "bar-42")
	if gotCwd != wantPath {
		t.Fatalf("worktree path = %q, want %q", gotCwd, wantPath)
	}
	if got := branchForWorktree(gotCwd); got != "gl/42-add-the-thing" {
		t.Errorf("worktree branch = %q, want gl/42-add-the-thing", got)
	}

	// Idempotent reuse: a second call returns the same path without
	// trying to re-add the worktree (which would fail).
	gotCwd2 := prepareTicketWorktree("github:foo/bar#42", clone, nil)
	if gotCwd2 != wantPath {
		t.Errorf("idempotent reuse returned %q, want %q", gotCwd2, wantPath)
	}
}

// When the title fetch returns "", the branch falls back to gl/<N>.
func TestPrepareTicketWorktree_EmptyTitleFallback(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "-q", "--bare", "-b", "main")
	upstream := initRepo(t)
	runGit(t, upstream, "remote", "add", "origin", bare)
	runGit(t, upstream, "push", "-q", "-u", "origin", "main")

	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	runGit(t, clone, "remote", "set-url", "origin", "https://github.com/foo/bar.git")

	home := t.TempDir()
	t.Setenv("HOME", home)

	gotCwd := prepareTicketWorktree("github:foo/bar#7", clone, func(_, _ string, _ int) string {
		return "" // simulate a fetch failure
	})
	if !strings.HasSuffix(gotCwd, "bar-7") {
		t.Errorf("worktree cwd = %q", gotCwd)
	}
	if got := branchForWorktree(gotCwd); got != "gl/7" {
		t.Errorf("branch = %q, want gl/7", got)
	}
}
