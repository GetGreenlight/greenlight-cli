//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// greenlightProvider is the built-in (Greenlight-owned) ticket backend (issue
// #176). Unlike githubProvider, the tickets live in permit-cloud's own DB, not
// an external tracker — so List/Read/Create/Update are each a device-scoped
// daemon-WS round-trip to the server (builtin_ticket_list / builtin_ticket_read
// / builtin_ticket_create / builtin_ticket_update) rather than an HTTP API call.
// The token argument is always "" (providerNeedsToken("greenlight") is false);
// repo_key is derived from owner/repo the same way the server keys
// tags/stages/autopilot, so a built-in ticket's whole workflow composes for free.
//
// The same code serves two callers, both via daemonWSRequest → daemon → server:
//   - the agent running `greenlight ticket …` in a session (a standalone process
//     that must reach the daemon over IPC anyway), and
//   - the daemon's own app-driven handlers (handleListTickets etc.), which run in
//     their own goroutine so the in-process IPC hop never blocks the read loop.
//
// Merge is NOT a provider-method concern for greenlight: a built-in ticket has no
// pull request, so `ticket merge` is a regular local git merge (current branch →
// default branch, push) that the command layer dispatches directly
// (runTicketMergeGreenlight) — it needs the repo cwd, which the TicketProvider
// interface doesn't carry. The Merge method below is therefore an unreachable
// guard.
type greenlightProvider struct{}

// glTicketTimeout bounds a single built-in ticket daemon-WS round-trip. These
// are local DB reads/writes, so they return fast; the generous ceiling only
// guards against a wedged daemon/server.
const glTicketTimeout = 30 * time.Second

// glRepoKey is the provider-canonical identity for a built-in ticket's repo,
// matching the server's repo_key and resolveTagTarget's lowercasing.
func glRepoKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

func (greenlightProvider) List(owner, repo, token string) ([]TicketSummary, error) {
	raw, err := daemonWSRequest("builtin_ticket_list", map[string]interface{}{
		"repo_key": glRepoKey(owner, repo),
	}, glTicketTimeout)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tickets []TicketSummary `json:"tickets"`
		Error   string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode builtin_ticket_list: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Tickets, nil
}

func (greenlightProvider) Read(owner, repo, token, id string) (*TicketDetail, error) {
	raw, err := daemonWSRequest("builtin_ticket_read", map[string]interface{}{
		"repo_key":  glRepoKey(owner, repo),
		"opaque_id": id,
	}, glTicketTimeout)
	if err != nil {
		return nil, err
	}
	return glParseDetail(raw, "builtin_ticket_read")
}

func (greenlightProvider) Create(owner, repo, token string, in TicketInput) (*TicketDetail, error) {
	raw, err := daemonWSRequest("builtin_ticket_create", map[string]interface{}{
		"repo_key": glRepoKey(owner, repo),
		"title":    in.Title,
		"body":     in.Body,
	}, glTicketTimeout)
	if err != nil {
		return nil, err
	}
	return glParseDetail(raw, "builtin_ticket_create")
}

func (greenlightProvider) Update(owner, repo, token, id string, patch TicketPatch) (*TicketDetail, error) {
	payload := map[string]interface{}{
		"repo_key":  glRepoKey(owner, repo),
		"opaque_id": id,
	}
	// Only send the fields being changed; the server leaves the rest untouched.
	if patch.Title != nil {
		payload["title"] = *patch.Title
	}
	if patch.Body != nil {
		payload["body"] = *patch.Body
	}
	if patch.State != nil {
		payload["state"] = *patch.State
	}
	raw, err := daemonWSRequest("builtin_ticket_update", payload, glTicketTimeout)
	if err != nil {
		return nil, err
	}
	return glParseDetail(raw, "builtin_ticket_update")
}

// Merge is an unreachable guard: the command layer dispatches a built-in ticket's
// merge to runTicketMergeGreenlight (local git) before ever calling a provider
// method, because the merge needs the repo cwd the interface doesn't carry.
func (greenlightProvider) Merge(owner, repo, token, id string, opts MergeOptions) (*MergeResult, error) {
	return nil, fmt.Errorf("merge_local")
}

// glParseDetail decodes a {ticket, error} daemon-WS reply into a TicketDetail.
func glParseDetail(raw json.RawMessage, op string) (*TicketDetail, error) {
	var resp struct {
		Ticket *TicketDetail `json:"ticket"`
		Error  string        `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode %s: %w", op, err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	if resp.Ticket == nil {
		return nil, fmt.Errorf("empty_ticket")
	}
	return resp.Ticket, nil
}

// gitRemoteSlug resolves (owner, repo) for the greenlight provider from the
// cwd's git repo for ANY git host (the greenlight provider isn't tied to
// github.com). Used only by the greenlight provider; github keeps its strict
// github.com-only parser.
//
// Resolution is remote-preferring so existing greenlight tickets keep their
// exact repo_key (issue #3), in order:
//  1. `origin` remote — unchanged from the original behavior.
//  2. the sole remote, if there is no `origin` but exactly one remote exists
//     (e.g. a repo whose only remote is `upstream`). Two-or-more remotes with no
//     `origin` skip to step 3 rather than guess.
//  3. a deterministic local identity derived from the git working-tree root
//     (localRepoKeyFor), for a repo with no usable remote at all.
//
// Only a cwd that is not inside any git working tree returns an error (no_repo).
// Note the stability caveat: a repo using the local fallback flips its repo_key
// if a remote is later added — see docs/greenlight-provider-spec.md §2.
func gitRemoteSlug(cwd string) (string, string, error) {
	// 1. origin remote (unchanged — preserves every existing ticket's repo_key).
	if out, err := exec.Command("git", "-C", cwd, "remote", "get-url", "origin").Output(); err == nil {
		if owner, repo, err := parseRemoteSlug(strings.TrimSpace(string(out))); err == nil {
			return owner, repo, nil
		}
	}
	// 2. sole non-origin remote, if exactly one remote exists.
	if remotes, err := gitRemotes(cwd); err == nil && len(remotes) == 1 {
		if out, err := exec.Command("git", "-C", cwd, "remote", "get-url", remotes[0]).Output(); err == nil {
			if owner, repo, err := parseRemoteSlug(strings.TrimSpace(string(out))); err == nil {
				return owner, repo, nil
			}
		}
	}
	// 3. local fallback identity (no usable remote). Errors only outside a git tree.
	return localRepoKeyFor(cwd)
}

// gitRemotes lists the cwd repo's configured remote names. Returns an error if
// cwd is not a git repo (so callers can distinguish "no remotes" — an empty
// slice with a nil error — from "not a repo").
func gitRemotes(cwd string) ([]string, error) {
	out, err := exec.Command("git", "-C", cwd, "remote").Output()
	if err != nil {
		return nil, err
	}
	var remotes []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			remotes = append(remotes, line)
		}
	}
	return remotes, nil
}

// localRepoKeyFor derives a deterministic, two-segment `owner/repo`-shaped
// identity for a remoteless greenlight repo (issue #3, step 3). The owner is the
// fixed string "local"; the repo is the sanitized lowercase basename of the git
// working-tree root suffixed with the first 8 hex chars of sha256(absolute
// toplevel path). The path-derived hash makes the key stable across sessions and
// unique per repo path (same-basename repos in different dirs don't collide); the
// sanitization keeps the repo segment within [a-z0-9._-] so the handle parsers
// (which split on `/` and truncate at the first `#`) round-trip it unchanged.
//
// Returns an error (no_repo) only when cwd is not inside a git working tree.
func localRepoKeyFor(cwd string) (string, string, error) {
	root, err := gitOut(cwd, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", "", fmt.Errorf("not a git working tree")
	}
	sum := sha256.Sum256([]byte(root))
	hash := hex.EncodeToString(sum[:])[:8]
	repo := sanitizeRepoSegment(filepath.Base(root))
	if repo == "" {
		// A basename that sanitizes to empty (root is "/", all-punctuation, …)
		// still needs a non-empty, readable repo segment.
		repo = "repo"
	}
	return "local", repo + "-" + hash, nil
}

// sanitizeRepoSegment lowercases s and maps any character outside [a-z0-9._-] to
// `-`, then collapses runs of `-` and trims leading/trailing `-`. This is the
// same character class the server's tag validator enforces, and it guarantees the
// result contains no `/` or `#` so it can't break the two-segment handle format.
func sanitizeRepoSegment(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// parseRemoteSlug extracts (owner, repo) from a git remote URL, handling the SSH
// (`git@host:owner/repo.git`, including host aliases and `ssh://host:port/…`)
// and HTTPS (`https://host/owner/repo.git`) forms. It takes the LAST two path
// segments as owner/repo, so an SSH port or a GitLab subgroup prefix is dropped
// (documented limitation: two repos that differ only by subgroup collide on
// repo_key). A trailing `.git` is stripped. Returns an error if fewer than two
// path segments are present.
func parseRemoteSlug(remote string) (string, string, error) {
	s := remote
	// Drop scheme://
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Drop user@ (e.g. git@)
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	// Split host from path on the first ':' (SSH) or '/' (HTTPS).
	i := strings.IndexAny(s, ":/")
	if i < 0 {
		return "", "", fmt.Errorf("remote has no path: %q", remote)
	}
	path := strings.Trim(s[i+1:], "/")
	path = strings.TrimSuffix(path, ".git")

	parts := []string{}
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote: %q", remote)
	}
	owner := parts[len(parts)-2]
	repo := parts[len(parts)-1]
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote: %q", remote)
	}
	return owner, repo, nil
}

// --- Local git merge (built-in `ticket merge`) -------------------------------
//
// A built-in ticket has no PR, so its merge is a plain local git merge of the
// work branch into the repo's default branch followed by a push. The CLI already
// runs inside the repo on the host, so this needs no token. See
// docs/greenlight-provider-spec.md §6.

// localMergeResult describes a completed local merge.
type localMergeResult struct {
	WorkBranch    string
	DefaultBranch string
	SHA           string // resulting default-branch HEAD
	Squashed      bool
}

// gitOut runs a git command in cwd and returns trimmed stdout. Git chatter on
// stderr is suppressed (callers that want it use gitRun).
func gitOut(cwd string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// gitRun runs a git command in cwd, sending all of git's own output to stderr so
// it never pollutes the payload stdout (the cli/CLAUDE.md "stdout carries the
// payload, nothing else" convention).
func gitRun(cwd string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gitDefaultBranch resolves the repo's default branch: origin/HEAD's target,
// falling back to a local main/master, then "main".
func gitDefaultBranch(cwd string) string {
	if ref, err := gitOut(cwd, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return strings.TrimPrefix(ref, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if _, err := gitOut(cwd, "rev-parse", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b
		}
	}
	return "main"
}

// gitCurrentBranch returns the checked-out branch name (or an error in a detached
// HEAD, where there is no work branch to merge).
func gitCurrentBranch(cwd string) (string, error) {
	b, err := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if b == "HEAD" {
		return "", fmt.Errorf("detached HEAD")
	}
	return b, nil
}

// gitWorkingTreeClean reports whether the working tree has no staged or unstaged
// changes (untracked files included).
func gitWorkingTreeClean(cwd string) (bool, error) {
	out, err := gitOut(cwd, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// gitBranchAhead reports whether work has at least one commit not in base.
func gitBranchAhead(cwd, base, work string) (bool, error) {
	out, err := gitOut(cwd, "rev-list", "--count", base+".."+work)
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(out)
	return n > 0, nil
}

// mergeGreenlightLocal merges work into base and pushes, leaving the repo on the
// work branch. Preconditions (clean tree, work != base, work ahead of base) are
// checked first and fail before any branch switch. On a merge conflict or a
// rejected push the local default branch is hard-reset to its pre-merge commit
// and the prior branch is restored, so a failure never leaves a half-merged
// state. Returns a wire-style error code on failure (dirty_tree /
// on_default_branch / not_ahead / checkout_failed / pull_failed / merge_conflict
// / push_failed) so the caller can map it to a human message.
func mergeGreenlightLocal(cwd, work, base, method string) (*localMergeResult, error) {
	clean, err := gitWorkingTreeClean(cwd)
	if err != nil {
		return nil, fmt.Errorf("no_repo")
	}
	if !clean {
		return nil, fmt.Errorf("dirty_tree")
	}
	if work == base {
		return nil, fmt.Errorf("on_default_branch")
	}
	if ahead, err := gitBranchAhead(cwd, base, work); err != nil {
		return nil, fmt.Errorf("no_repo")
	} else if !ahead {
		return nil, fmt.Errorf("not_ahead")
	}

	if err := gitRun(cwd, "checkout", base); err != nil {
		return nil, fmt.Errorf("checkout_failed")
	}
	restore := func() { _ = gitRun(cwd, "checkout", work) }

	if err := gitRun(cwd, "pull", "--ff-only"); err != nil {
		restore()
		return nil, fmt.Errorf("pull_failed")
	}
	// Capture the pre-merge base commit so a failed merge/push can be undone
	// cleanly regardless of merge method (--no-ff commit or --squash staging).
	baseSHA, err := gitOut(cwd, "rev-parse", "HEAD")
	if err != nil {
		restore()
		return nil, fmt.Errorf("no_repo")
	}

	var mergeErr error
	if method == "squash" {
		if mergeErr = gitRun(cwd, "merge", "--squash", work); mergeErr == nil {
			mergeErr = gitRun(cwd, "commit", "-m", fmt.Sprintf("Merge branch '%s' (squash)", work))
		}
	} else {
		mergeErr = gitRun(cwd, "merge", "--no-ff", "-m", fmt.Sprintf("Merge branch '%s'", work), work)
	}
	if mergeErr != nil {
		_ = gitRun(cwd, "reset", "--hard", baseSHA)
		restore()
		return nil, fmt.Errorf("merge_conflict")
	}

	if err := gitRun(cwd, "push", "origin", base); err != nil {
		_ = gitRun(cwd, "reset", "--hard", baseSHA)
		restore()
		return nil, fmt.Errorf("push_failed")
	}

	sha, _ := gitOut(cwd, "rev-parse", "HEAD")
	restore()
	return &localMergeResult{WorkBranch: work, DefaultBranch: base, SHA: sha, Squashed: method == "squash"}, nil
}
