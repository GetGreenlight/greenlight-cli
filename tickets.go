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
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	HTMLURL   string   `json:"html_url"`
	UpdatedAt string   `json:"updated_at"`
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
