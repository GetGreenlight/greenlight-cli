//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var ticketRefRE = regexp.MustCompile(`^github:([^/]+)/([^#]+)#(\d+)$`)

// parseTicketRef matches refs of the form `github:owner/repo#N`.
// Returns ok=false for any other shape so the caller can fall through.
func parseTicketRef(ticket string) (owner, repo string, number int, ok bool) {
	m := ticketRefRE.FindStringSubmatch(ticket)
	if m == nil {
		return "", "", 0, false
	}
	if _, err := fmt.Sscanf(m[3], "%d", &number); err != nil || number <= 0 {
		return "", "", 0, false
	}
	return m[1], m[2], number, true
}

// slugifyTitle turns an issue title into a branch-safe slug.
// Lowercases, replaces runs of non-alphanumerics with a single dash,
// trims dashes, and caps the length at 40 chars (cutting at a dash boundary
// when possible to avoid truncating mid-word in a weird-looking way).
func slugifyTitle(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	prevDash := true
	for _, r := range strings.ToLower(title) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	const maxLen = 40
	if len(s) > maxLen {
		s = s[:maxLen]
		if i := strings.LastIndexByte(s, '-'); i > 0 {
			s = s[:i]
		}
	}
	return s
}

// branchNameForTicket builds the branch name for a ticket worktree.
// Empty title (or one that slugifies to "") falls back to "gl/<N>".
func branchNameForTicket(number int, title string) string {
	slug := slugifyTitle(title)
	if slug == "" {
		return fmt.Sprintf("gl/%d", number)
	}
	return fmt.Sprintf("gl/%d-%s", number, slug)
}

// worktreePathForTicket returns the predictable global worktree path
// (~/.greenlight/worktrees/<repo>-<N>/). The empty string is returned
// if HOME cannot be resolved — callers fall back to no-worktree behaviour.
func worktreePathForTicket(repo string, number int) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".greenlight", "worktrees", fmt.Sprintf("%s-%d", repo, number))
}

// defaultBranchFromRemote returns the repo's default branch by reading
// refs/remotes/origin/HEAD (no network call). Falls back to "main" if
// the symbolic ref can't be resolved.
func defaultBranchFromRemote(cwd string) string {
	cmd := exec.Command("git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "main"
	}
	// output looks like "origin/main"
	ref := strings.TrimSpace(string(out))
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// prepareTicketWorktree creates (or reuses) a worktree for a ticket-scoped
// session. Returns the worktree path on success, or the original cwd
// unchanged on any failure. Failures are logged but never block session
// startup — the user still gets a session, just without the worktree.
//
// Behaviour:
//   - parses ticket as github:owner/repo#N
//   - confirms cwd is a git repo whose origin matches owner/repo
//   - if ~/.greenlight/worktrees/<repo>-<N>/ already exists and is a git
//     worktree, reuses it (idempotent)
//   - otherwise: `git worktree add -b gl/<N>-<slug> <path> origin/<default>`
//     (slug derived from the issue title via fetchIssueTitle; on title fetch
//     failure, slug is empty so branch is just "gl/<N>")
func prepareTicketWorktree(ticket, cwd string, fetchIssueTitle func(owner, repo string, number int) string) string {
	owner, repo, number, ok := parseTicketRef(ticket)
	if !ok {
		return cwd
	}

	gotOwner, gotRepo, err := repoFromCwd(cwd)
	if err != nil {
		log.Printf("worktree: cwd is not a github repo: %v", err)
		return cwd
	}
	if !strings.EqualFold(gotOwner, owner) || !strings.EqualFold(gotRepo, repo) {
		log.Printf("worktree: cwd origin %s/%s != ticket %s/%s, skipping", gotOwner, gotRepo, owner, repo)
		return cwd
	}

	wtPath := worktreePathForTicket(repo, number)
	if wtPath == "" {
		log.Printf("worktree: could not resolve ~/.greenlight/worktrees path")
		return cwd
	}

	// Idempotent reuse: if the path already looks like a worktree, use it.
	if info, err := os.Stat(wtPath); err == nil && info.IsDir() {
		check := exec.Command("git", "-C", wtPath, "rev-parse", "--is-inside-work-tree")
		if out, err := check.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
			log.Printf("worktree: reusing existing %s", wtPath)
			return wtPath
		}
		log.Printf("worktree: path %s exists but isn't a worktree, leaving cwd as-is", wtPath)
		return cwd
	}

	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		log.Printf("worktree: mkdir parent: %v", err)
		return cwd
	}

	defaultBranch := defaultBranchFromRemote(cwd)
	var title string
	if fetchIssueTitle != nil {
		title = fetchIssueTitle(owner, repo, number)
	}
	branch := branchNameForTicket(number, title)

	args := []string{"-C", cwd, "worktree", "add", "-b", branch, wtPath, "origin/" + defaultBranch}
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("worktree: git worktree add failed: %v: %s", err, strings.TrimSpace(string(out)))
		return cwd
	}
	log.Printf("worktree: created %s on branch %s (base origin/%s)", wtPath, branch, defaultBranch)
	return wtPath
}

// branchForWorktree returns the current branch name in the given worktree,
// or "" on error. Used by open_pr to know what to push.
func branchForWorktree(cwd string) string {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
