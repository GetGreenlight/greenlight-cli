//go:build darwin || linux

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// githubAPIBase is the GitHub REST API root. Overridable via the
// GREENLIGHT_GITHUB_API_BASE env var so integration tests can point at a
// local httptest.Server. Production never sets this.
func githubAPIBase() string {
	if v := os.Getenv("GREENLIGHT_GITHUB_API_BASE"); v != "" {
		return v
	}
	return "https://api.github.com"
}

type ticket struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Labels    []string  `json:"labels"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt string    `json:"updated_at"`
	LinkedPR  *linkedPR `json:"linked_pr,omitempty"`
}

// linkedPR is the basic v2.6.0 form of the linked-PR metadata.
// v2.6.1 will add mergeable_state/checks_conclusion/review_decision via a
// per-PR detail call; for now iOS renders whatever's present.
type linkedPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
}

// handleListTickets resolves the session's repo from its cwd, fetches open
// issues from GitHub (filtering out pull requests), and replies with a
// `tickets_listed` frame. State defaults to "open".
//
// The actual work runs in a goroutine because fetchGitHubToken issues a
// SendRequest that waits for a server response over the same WS this
// handler is dispatched from — running synchronously would deadlock the
// read loop.
func (d *DaemonWS) handleListTickets(data []byte) {
	var msg struct {
		RelayID string `json:"relay_id"`
		State   string `json:"state"` // open | closed | all; default open
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" {
		log.Printf("daemon-ws: list_tickets missing relay_id")
		return
	}
	state := msg.State
	if state == "" {
		state = "open"
	}
	go d.serveListTickets(msg.RelayID, state)
}

func (d *DaemonWS) serveListTickets(relayID, state string) {
	d.mu.RLock()
	sw := d.sessions[relayID]
	d.mu.RUnlock()
	if sw == nil {
		d.sendTicketsListed(relayID, "", "", nil, "unknown session")
		return
	}

	owner, repo, err := repoFromCwd(sw.cwd)
	if err != nil {
		d.sendTicketsListed(relayID, "", "", nil, err.Error())
		return
	}

	token, err := d.fetchGitHubToken()
	if err != nil {
		d.sendTicketsListed(relayID, owner, repo, nil, fmt.Sprintf("token: %v", err))
		return
	}

	items, err := fetchGitHubIssues(string(token), owner, repo, state)
	if err != nil {
		d.sendTicketsListed(relayID, owner, repo, nil, err.Error())
		return
	}

	// Enrich with linked_pr metadata. Failures here are non-fatal: we still
	// return the tickets, just without the In-Review column information.
	if prs, err := fetchOpenGitHubPRs(string(token), owner, repo); err != nil {
		log.Printf("daemon-ws: linked_pr enrichment failed: %v", err)
	} else {
		attachLinkedPRs(items, prs)
	}

	d.sendTicketsListed(relayID, owner, repo, items, "")
}

func (d *DaemonWS) sendTicketsListed(relayID, owner, repo string, items []ticket, errMsg string) {
	if items == nil {
		items = []ticket{}
	}
	resp := map[string]interface{}{
		"type":     "tickets_listed",
		"relay_id": relayID,
		"owner":    owner,
		"repo":     repo,
		"tickets":  items,
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	out, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: marshal tickets_listed: %v", err)
		return
	}
	d.ws.SendText(out)
}

// fetchGitHubToken pulls GITHUB_ACCESS_TOKEN from greenlight secrets via the
// daemon WS and decrypts it locally.
func (d *DaemonWS) fetchGitHubToken() ([]byte, error) {
	reqID := generateUUID()
	raw, err := d.SendRequest("secrets_get", reqID, map[string]interface{}{
		"request_id": reqID,
		"key":        "GITHUB_ACCESS_TOKEN",
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Ciphertext string `json:"ciphertext"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error == "not_found" {
		return nil, fmt.Errorf("GITHUB_ACCESS_TOKEN not set; run `greenlight secrets set GITHUB_ACCESS_TOKEN`")
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
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

// repoFromCwd discovers a GitHub owner/repo from `git remote get-url origin`
// in cwd. Returns an error if the directory isn't a git repo or origin
// isn't a github.com URL.
func repoFromCwd(cwd string) (string, string, error) {
	if cwd == "" {
		return "", "", fmt.Errorf("session has no cwd")
	}
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	owner, repo, ok := parseGitHubRemote(strings.TrimSpace(string(out)))
	if !ok {
		return "", "", fmt.Errorf("origin %q is not a github.com remote", strings.TrimSpace(string(out)))
	}
	return owner, repo, nil
}

// parseGitHubRemote extracts owner/repo from a git remote URL.
// Handles:
//
//	https://github.com/owner/repo[.git]
//	git@github.com:owner/repo[.git]
//	ssh://git@github.com/owner/repo[.git]
func parseGitHubRemote(remote string) (string, string, bool) {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, ".git")

	// SCP-style: git@host:owner/repo
	if at := strings.Index(remote, "@"); at >= 0 && strings.Contains(remote, ":") && !strings.HasPrefix(remote, "ssh://") {
		host, path, ok := strings.Cut(remote[at+1:], ":")
		if !ok || !strings.EqualFold(host, "github.com") {
			return "", "", false
		}
		return splitOwnerRepo(path)
	}

	u, err := url.Parse(remote)
	if err != nil {
		return "", "", false
	}
	if !strings.EqualFold(u.Host, "github.com") {
		return "", "", false
	}
	return splitOwnerRepo(strings.TrimPrefix(u.Path, "/"))
}

func splitOwnerRepo(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// fetchGitHubIssues calls GET /repos/<owner>/<repo>/issues and filters out
// pull requests (which share the issue endpoint and number space).
func fetchGitHubIssues(token, owner, repo, state string) ([]ticket, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues?state=%s&per_page=100",
		strings.TrimRight(githubAPIBase(), "/"),
		url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(state))
	req, err := http.NewRequest("GET", u, nil)
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
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("github %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return parseIssuesResponse(body)
}

// fetchGitHubIssueTitle returns the title of a single issue (or PR).
// Used by the ticket-worktree path to slug the branch name. Returns ""
// (and a non-nil error) on any failure — callers fall back to a slugless branch.
func fetchGitHubIssueTitle(token, owner, repo string, number int) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d",
		strings.TrimRight(githubAPIBase(), "/"),
		url.PathEscape(owner), url.PathEscape(repo), number)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("github %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.Title, nil
}

// parseIssuesResponse decodes the GitHub /issues response and drops pull
// requests. Split out from fetchGitHubIssues so the PR-filter — the
// gotcha the API actually has — can be tested without an HTTP fixture.
func parseIssuesResponse(body []byte) ([]ticket, error) {
	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		HTMLURL     string `json:"html_url"`
		UpdatedAt   string `json:"updated_at"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}

	out := make([]ticket, 0, len(raw))
	for _, r := range raw {
		if len(r.PullRequest) > 0 && string(r.PullRequest) != "null" {
			continue // skip PRs
		}
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, ticket{
			Number:    r.Number,
			Title:     r.Title,
			State:     r.State,
			Labels:    labels,
			HTMLURL:   r.HTMLURL,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// rawPR is the subset of GitHub's PR response we need for linked_pr matching.
type rawPR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
}

// fetchOpenGitHubPRs lists open PRs in a repo (up to 100). The caller uses
// title + body to look for closing keywords against the issue list.
func fetchOpenGitHubPRs(token, owner, repo string) ([]rawPR, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100",
		strings.TrimRight(githubAPIBase(), "/"),
		url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequest("GET", u, nil)
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
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("github %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []rawPR
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse pulls: %w", err)
	}
	return out, nil
}

// closingKeywordRE matches GitHub's closing keywords + issue reference:
// (close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved) #N.
// The form `owner/repo#N` (cross-repo) is intentionally not matched here —
// the issue scopes cross-repo linkage to a future phase.
var closingKeywordRE = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// parseClosingKeywords pulls every issue number referenced by a closing
// keyword out of free-form PR text (title + body).
func parseClosingKeywords(text string) []int {
	matches := closingKeywordRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(matches))
	out := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// attachLinkedPRs walks the PR list, parses closing keywords out of each
// PR's title+body, and attaches a linkedPR to every matching issue.
// Non-draft PRs win over drafts; otherwise first match wins.
func attachLinkedPRs(items []ticket, prs []rawPR) {
	if len(items) == 0 || len(prs) == 0 {
		return
	}
	idx := make(map[int]*ticket, len(items))
	for i := range items {
		idx[items[i].Number] = &items[i]
	}
	for _, pr := range prs {
		text := pr.Title + "\n" + pr.Body
		for _, n := range parseClosingKeywords(text) {
			t, ok := idx[n]
			if !ok {
				continue
			}
			// Prefer non-draft when overwriting an existing match.
			if t.LinkedPR != nil && t.LinkedPR.Draft == pr.Draft {
				continue // keep first match at same draft status
			}
			if t.LinkedPR != nil && !t.LinkedPR.Draft && pr.Draft {
				continue // already have a non-draft, don't downgrade
			}
			t.LinkedPR = &linkedPR{
				Number:  pr.Number,
				HTMLURL: pr.HTMLURL,
				State:   pr.State,
				Draft:   pr.Draft,
			}
		}
	}
}
