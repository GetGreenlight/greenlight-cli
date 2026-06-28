//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemoteSlug(t *testing.T) {
	cases := []struct {
		name, remote, owner, repo string
		wantErr                   bool
	}{
		{"ssh", "git@github.com:getgreenlight/permit.git", "getgreenlight", "permit", false},
		{"ssh no .git", "git@github.com:owner/repo", "owner", "repo", false},
		{"ssh host alias", "git@github.com-personal:owner/repo.git", "owner", "repo", false},
		{"https", "https://github.com/getgreenlight/permit.git", "getgreenlight", "permit", false},
		{"https no .git", "https://gitlab.com/owner/repo", "owner", "repo", false},
		{"https trailing slash", "https://example.com/owner/repo/", "owner", "repo", false},
		{"ssh url with port", "ssh://git@host.example.com:2222/owner/repo.git", "owner", "repo", false},
		{"self-hosted https", "https://git.internal.corp/team/service.git", "team", "service", false},
		{"gitlab subgroup takes last two", "https://gitlab.com/group/sub/repo.git", "sub", "repo", false},
		{"dotted repo name", "https://github.com/user/user.github.io.git", "user", "user.github.io", false},
		{"no path", "git@github.com", "", "", true},
		{"single segment", "https://github.com/owneronly", "", "", true},
		{"empty", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, repo, err := parseRemoteSlug(c.remote)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q/%q", owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != c.owner || repo != c.repo {
				t.Errorf("got %q/%q, want %q/%q", owner, repo, c.owner, c.repo)
			}
		})
	}
}

// --- ordered repo resolution for the greenlight provider (issue #3) ----------

// repoNoOrigin builds a bare temp git repo (no remote, no clone) with one commit
// on branch `main`, at the given basename under a fresh temp dir. Returns the
// work-tree path.
func repoNoOrigin(t *testing.T, basename string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, basename)
	mustGit(t, root, "init", "-b", "main", work)
	mustGit(t, work, "config", "commit.gpgsign", "false")
	mustGit(t, work, "config", "user.email", "t@t")
	mustGit(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "README.md")
	mustGit(t, work, "commit", "-m", "init")
	return work
}

func TestGitRemoteSlug_Resolution(t *testing.T) {
	t.Run("origin remote is byte-identical to the original behavior", func(t *testing.T) {
		work := repoWithOrigin(t)
		mustGit(t, work, "remote", "set-url", "origin", "git@github.com:getgreenlight/permit.git")
		owner, repo, err := gitRemoteSlug(work)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if owner != "getgreenlight" || repo != "permit" {
			t.Errorf("got %q/%q, want getgreenlight/permit", owner, repo)
		}
	})

	t.Run("sole non-origin remote (only upstream)", func(t *testing.T) {
		work := repoNoOrigin(t, "widget")
		mustGit(t, work, "remote", "add", "upstream", "https://gitlab.com/acme/gadget.git")
		owner, repo, err := gitRemoteSlug(work)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if owner != "acme" || repo != "gadget" {
			t.Errorf("got %q/%q, want acme/gadget", owner, repo)
		}
	})

	t.Run("no remotes falls back to local identity", func(t *testing.T) {
		work := repoNoOrigin(t, "widget")
		owner, repo, err := gitRemoteSlug(work)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if owner != "local" {
			t.Errorf("owner=%q, want local", owner)
		}
		if !strings.HasPrefix(repo, "widget-") {
			t.Errorf("repo=%q, want widget-<hash>", repo)
		}
	})

	t.Run("two non-origin remotes skip to local identity", func(t *testing.T) {
		work := repoNoOrigin(t, "widget")
		mustGit(t, work, "remote", "add", "upstream", "https://gitlab.com/acme/gadget.git")
		mustGit(t, work, "remote", "add", "fork", "https://gitlab.com/me/gadget.git")
		owner, repo, err := gitRemoteSlug(work)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if owner != "local" || !strings.HasPrefix(repo, "widget-") {
			t.Errorf("got %q/%q, want local/widget-<hash>", owner, repo)
		}
	})

	t.Run("non-git directory returns an error (no_repo)", func(t *testing.T) {
		dir := t.TempDir()
		if owner, repo, err := gitRemoteSlug(dir); err == nil {
			t.Errorf("expected error for non-git dir, got %q/%q", owner, repo)
		}
	})
}

func TestLocalRepoKeyFor(t *testing.T) {
	t.Run("deterministic across calls", func(t *testing.T) {
		work := repoNoOrigin(t, "widget")
		o1, r1, err := localRepoKeyFor(work)
		if err != nil {
			t.Fatal(err)
		}
		o2, r2, err := localRepoKeyFor(work)
		if err != nil {
			t.Fatal(err)
		}
		if o1 != o2 || r1 != r2 {
			t.Errorf("non-deterministic: %q/%q vs %q/%q", o1, r1, o2, r2)
		}
	})

	t.Run("same basename in different dirs yields different keys", func(t *testing.T) {
		a := repoNoOrigin(t, "widget")
		b := repoNoOrigin(t, "widget")
		_, ra, err := localRepoKeyFor(a)
		if err != nil {
			t.Fatal(err)
		}
		_, rb, err := localRepoKeyFor(b)
		if err != nil {
			t.Fatal(err)
		}
		if ra == rb {
			t.Errorf("same-basename repos in different dirs collided on %q", ra)
		}
	})

	t.Run("two clean non-empty segments with no # for a messy basename", func(t *testing.T) {
		work := repoNoOrigin(t, "My Project#42")
		owner, repo, err := localRepoKeyFor(work)
		if err != nil {
			t.Fatal(err)
		}
		key := owner + "/" + repo
		parts := strings.Split(key, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			t.Fatalf("key %q is not two non-empty /-segments", key)
		}
		if strings.Contains(repo, "#") {
			t.Errorf("repo segment %q contains '#' — breaks the handle round-trip", repo)
		}
		// Sanity: the messy basename was sanitized to the [a-z0-9._-] class.
		if !strings.HasPrefix(repo, "my-project-42-") {
			t.Errorf("repo=%q, want my-project-42-<hash> prefix", repo)
		}
	})
}

func TestSanitizeRepoSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"widget", "widget"},
		{"My Project", "my-project"},
		{"my#thing", "my-thing"},
		{"user.github.io", "user.github.io"},
		{"a--b__c", "a-b__c"},
		{"--lead-trail--", "lead-trail"},
		{"###", ""},
		{"CAFÉ", "caf"},
	}
	for _, c := range cases {
		if got := sanitizeRepoSegment(c.in); got != c.want {
			t.Errorf("sanitizeRepoSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGLRepoKey_Lowercases(t *testing.T) {
	if got := glRepoKey("GetGreenlight", "Permit"); got != "getgreenlight/permit" {
		t.Errorf("glRepoKey = %q, want getgreenlight/permit", got)
	}
}

func TestGreenlightProviderRegistration(t *testing.T) {
	if !knownTicketProviders["greenlight"] {
		t.Error("greenlight not in knownTicketProviders (won't be config-settable)")
	}
	prov, ok := providerFor("greenlight")
	if !ok {
		t.Fatal("providerFor(greenlight) returned ok=false")
	}
	if _, isGL := prov.(greenlightProvider); !isGL {
		t.Errorf("providerFor(greenlight) = %T, want greenlightProvider", prov)
	}
}

func TestProviderNeedsToken(t *testing.T) {
	if !providerNeedsToken("github") {
		t.Error("github should need a token")
	}
	if providerNeedsToken("greenlight") {
		t.Error("greenlight should NOT need a token (server-stored)")
	}
}

// The provider Merge method is an unreachable guard: a built-in ticket's merge is
// a local git merge dispatched at the command layer (runTicketMergeGreenlight),
// not via the provider interface (which can't carry the repo cwd it needs).
func TestGreenlightProviderMergeIsCommandLayer(t *testing.T) {
	_, err := greenlightProvider{}.Merge("o", "r", "", "1", MergeOptions{})
	if err == nil || err.Error() != "merge_local" {
		t.Errorf("Merge err = %v, want merge_local", err)
	}
}

// --- default-when-unset provider flip (#176 decision #5) ---------------------

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Deterministic identity + no signing/hooks so the test is hermetic.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWithOrigin builds a temp work repo on branch `main` whose `origin` is a
// local bare repo, with an initial commit pushed and origin/HEAD → origin/main.
// Returns the work-tree path.
func repoWithOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	mustGit(t, root, "init", "--bare", "-b", "main", origin)
	mustGit(t, root, "clone", origin, work)
	mustGit(t, work, "config", "commit.gpgsign", "false")
	mustGit(t, work, "config", "user.email", "t@t")
	mustGit(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "README.md")
	mustGit(t, work, "commit", "-m", "init")
	mustGit(t, work, "push", "origin", "main")
	// origin/HEAD so gitDefaultBranch resolves to main.
	mustGit(t, work, "remote", "set-head", "origin", "main")
	return work
}

func TestResolveTicketEnv_DefaultsToGreenlight(t *testing.T) {
	// Isolate config to an empty HOME so tickets_provider is unset.
	t.Setenv("HOME", t.TempDir())
	work := repoWithOrigin(t)

	env, provider, errCode := resolveTicketEnv("proj", work)
	if errCode != "" {
		t.Fatalf("errCode=%q, want empty (default greenlight should resolve with no token)", errCode)
	}
	if provider != "greenlight" {
		t.Errorf("provider=%q, want greenlight (default when unset)", provider)
	}
	if env == nil || env.token != "" {
		t.Errorf("env=%v, want non-nil with empty token", env)
	}
}

// --- local git merge (#176 §6) ----------------------------------------------

func TestMergeGreenlightLocal_HappyPath(t *testing.T) {
	work := repoWithOrigin(t)
	originMainBefore := mustGit(t, work, "rev-parse", "origin/main")

	mustGit(t, work, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", "f.txt")
	mustGit(t, work, "commit", "-m", "feature work")

	res, err := mergeGreenlightLocal(work, "feature", "main", "")
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if res.WorkBranch != "feature" || res.DefaultBranch != "main" {
		t.Errorf("result branches: %+v", res)
	}
	// origin/main advanced (the push landed).
	if after := mustGit(t, work, "rev-parse", "origin/main"); after == originMainBefore {
		t.Errorf("origin/main did not advance after merge")
	}
	// Left on the work branch.
	if cur := mustGit(t, work, "rev-parse", "--abbrev-ref", "HEAD"); cur != "feature" {
		t.Errorf("left on %q, want feature", cur)
	}
	// The merge commit reached origin/main and contains the feature file.
	if files := mustGit(t, work, "ls-tree", "--name-only", "origin/main"); !strings.Contains(files, "f.txt") {
		t.Errorf("origin/main missing f.txt; tree:\n%s", files)
	}
}

func TestMergeGreenlightLocal_Guards(t *testing.T) {
	t.Run("dirty tree", func(t *testing.T) {
		work := repoWithOrigin(t)
		mustGit(t, work, "checkout", "-b", "feature")
		if err := os.WriteFile(filepath.Join(work, "dirty.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, work, "add", "dirty.txt")
		mustGit(t, work, "commit", "-m", "c")
		// Now make the tree dirty.
		if err := os.WriteFile(filepath.Join(work, "dirty.txt"), []byte("y\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := mergeGreenlightLocal(work, "feature", "main", ""); err == nil || err.Error() != "dirty_tree" {
			t.Errorf("err=%v, want dirty_tree", err)
		}
	})

	t.Run("on default branch", func(t *testing.T) {
		work := repoWithOrigin(t)
		if _, err := mergeGreenlightLocal(work, "main", "main", ""); err == nil || err.Error() != "on_default_branch" {
			t.Errorf("err=%v, want on_default_branch", err)
		}
	})

	t.Run("not ahead", func(t *testing.T) {
		work := repoWithOrigin(t)
		// A branch with no commits beyond main.
		mustGit(t, work, "checkout", "-b", "stale")
		if _, err := mergeGreenlightLocal(work, "stale", "main", ""); err == nil || err.Error() != "not_ahead" {
			t.Errorf("err=%v, want not_ahead", err)
		}
	})
}

func TestGitDefaultBranch(t *testing.T) {
	work := repoWithOrigin(t)
	if b := gitDefaultBranch(work); b != "main" {
		t.Errorf("gitDefaultBranch=%q, want main", b)
	}
}
