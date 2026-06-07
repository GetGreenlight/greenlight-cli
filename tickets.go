//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TicketRef is the provider-agnostic reference to an issue/ticket. Mirrors
// the server-side struct; widened from the bare-string form at CLI v2.6.
type TicketRef struct {
	Provider string `json:"provider"`
	OpaqueID string `json:"opaque_id"`
	URL      string `json:"url"`
}

// TicketSummary is what the UI consumes for the Tickets tab.
type TicketSummary struct {
	OpaqueID     string `json:"opaque_id"`
	Title        string `json:"title"`
	DisplayLabel string `json:"display_label"` // raw provider state (e.g. "open")
	CoarseState  string `json:"coarse_state"`  // reduced: "open" or "closed"
	URL          string `json:"url"`
}

// maxTicketsPerState caps how many tickets we return from each state list.
// GitHub repos with many issues otherwise blow up the wire payload.
const maxTicketsPerState = 100

// knownTicketProviders is the set of providers the CLI can fetch tickets for.
// It doubles as the validation allowlist for the `tickets_provider` config key
// (see validateConfigBatch). Only github is implemented today; adding a provider
// here also makes it config-settable.
var knownTicketProviders = map[string]bool{
	"github": true,
}

// handleListTickets resolves the project's cwd, parses the GitHub remote,
// fetches open + closed issues via the GitHub API, and replies with
// tickets_listed. Errors are returned as a non-empty `error` field with an
// empty tickets array — the UI renders these as banners.
func (d *DaemonWS) handleListTickets(data []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Project   string `json:"project"`
		Provider  string `json:"provider"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("daemon-ws: list_tickets: invalid JSON: %v", err)
		return
	}
	if msg.RequestID == "" {
		log.Printf("daemon-ws: list_tickets: missing request_id")
		return
	}

	reply := func(provider, owner, repo string, tickets []TicketSummary, errMsg string) {
		if tickets == nil {
			tickets = []TicketSummary{}
		}
		resp := map[string]interface{}{
			"type":       "tickets_listed",
			"request_id": msg.RequestID,
			"provider":   provider,
			"tickets":    tickets,
		}
		if owner != "" {
			resp["owner"] = owner
		}
		if repo != "" {
			resp["repo"] = repo
		}
		if errMsg != "" {
			resp["error"] = errMsg
		}
		out, err := json.Marshal(resp)
		if err != nil {
			log.Printf("daemon-ws: list_tickets: marshal tickets_listed: %v", err)
			return
		}
		d.ws.SendText(out)
	}

	if msg.Project == "" {
		reply("", "", "", nil, "missing project")
		return
	}

	// Provider and secret are config-driven (project override → host) with NO
	// built-in default: tickets stay off until the user configures them. The
	// message's provider field is ignored — config is the source of truth.
	provider := resolveConfig(msg.Project, configKeyTicketsProvider)
	if provider == "" {
		reply("", "", "", nil, "not_configured")
		return
	}
	if provider != "github" {
		reply(provider, "", "", nil, "unsupported provider")
		return
	}

	// The token secret name is config-driven; without one, tickets aren't set up.
	secretName := resolveConfig(msg.Project, configKeyTicketsSecret)
	if secretName == "" {
		reply(provider, "", "", nil, "not_configured")
		return
	}

	cwd := d.resolveProjectCwd(msg.Project)
	if cwd == "" {
		reply(provider, "", "", nil, "no_repo")
		return
	}

	owner, repo, err := gitRemoteOwnerRepo(cwd)
	if err != nil {
		log.Printf("daemon-ws: list_tickets: repo resolution failed for project %q cwd %q: %v", msg.Project, cwd, err)
		reply(provider, "", "", nil, "no_repo")
		return
	}
	log.Printf("daemon-ws: list_tickets: resolved %s/%s for project %q cwd %q", owner, repo, msg.Project, cwd)

	token, err := fetchAndDecrypt(secretName)
	if err != nil {
		log.Printf("daemon-ws: list_tickets: %s: %v", secretName, err)
		reply(provider, owner, repo, nil, "missing_token")
		return
	}

	open, errOpen := fetchGitHubIssues(owner, repo, "open", string(token))
	if errOpen != nil {
		log.Printf("daemon-ws: list_tickets: open fetch failed: %v", errOpen)
		reply(provider, owner, repo, nil, errOpen.Error())
		return
	}
	closed, errClosed := fetchGitHubIssues(owner, repo, "closed", string(token))
	if errClosed != nil {
		log.Printf("daemon-ws: list_tickets: closed fetch failed: %v", errClosed)
		reply(provider, owner, repo, nil, errClosed.Error())
		return
	}

	tickets := append(open, closed...)
	reply(provider, owner, repo, tickets, "")
	log.Printf("daemon-ws: list_tickets: %s/%s → %d tickets (project=%q)", owner, repo, len(tickets), msg.Project)
}

// resolveProjectCwd finds the cwd for a project name. Prefers a live session;
// falls back to the most recent persisted session record. Returns "" if no
// match.
func (d *DaemonWS) resolveProjectCwd(project string) string {
	d.mu.RLock()
	for _, sw := range d.sessions {
		if sw.project == project && sw.cwd != "" {
			cwd := sw.cwd
			d.mu.RUnlock()
			return cwd
		}
	}
	d.mu.RUnlock()
	// listSessionRecords returns newest-first.
	for _, rec := range listSessionRecords() {
		if rec.Project == project && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// Matches both the canonical github.com host and SSH-config aliases like
// "github.com-personal" that map to it via ~/.ssh/config. Repo names may
// contain dots (e.g. user.github.io for GitHub Pages); a trailing `.git`
// or `/` is stripped after capture.
var gitHubRemoteRE = regexp.MustCompile(`github\.com[^/:]*[:/]([^/]+)/([^/\s]+)`)

// gitRemoteOwnerRepo runs `git -C cwd remote get-url origin` and parses
// owner/repo out of a github.com URL. Handles both SSH and HTTPS forms.
func gitRemoteOwnerRepo(cwd string) (string, string, error) {
	cmd := exec.Command("git", "-C", cwd, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git remote get-url: %w", err)
	}
	remote := strings.TrimSpace(string(out))
	m := gitHubRemoteRE.FindStringSubmatch(remote)
	if m == nil {
		return "", "", fmt.Errorf("remote is not github.com: %q", remote)
	}
	repo := strings.TrimSuffix(m[2], ".git")
	repo = strings.TrimSuffix(repo, "/")
	return m[1], repo, nil
}

// fetchGitHubIssues calls the issues endpoint, filters out PRs, and converts
// to TicketSummary. Capped at maxTicketsPerState entries.
func fetchGitHubIssues(owner, repo, state, token string) ([]TicketSummary, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=%s&per_page=%d&sort=updated&direction=desc",
		url.PathEscape(owner), url.PathEscape(repo), state, maxTicketsPerState)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 403 with rate-limit headers means rate limited; otherwise token issue.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, fmt.Errorf("rate_limited")
		}
		return nil, fmt.Errorf("missing_token")
	}
	if resp.StatusCode == http.StatusNotFound {
		// GitHub returns 404 for both "doesn't exist" and "private repo the
		// token can't see" — they're intentionally indistinguishable.
		return nil, fmt.Errorf("repo_not_found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		HTMLURL     string `json:"html_url"`
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode issues: %w", err)
	}

	out := make([]TicketSummary, 0, len(raw))
	for _, it := range raw {
		if it.PullRequest != nil {
			continue
		}
		out = append(out, TicketSummary{
			OpaqueID:     strconv.Itoa(it.Number),
			Title:        it.Title,
			DisplayLabel: it.State,
			CoarseState:  coarseGitHubState(it.State),
			URL:          it.HTMLURL,
		})
	}
	return out, nil
}

// coarseGitHubState collapses GitHub's `open` / `closed` values to the
// shape the UI consumes. Provider-quarantined: any state not "open" is
// treated as closed/settled.
func coarseGitHubState(state string) string {
	if state == "open" {
		return "open"
	}
	return "closed"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
