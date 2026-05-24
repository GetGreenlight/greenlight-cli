//go:build darwin || linux

package main

import (
	"bytes"
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

// glBranchRE matches the branch name we create in prepareTicketWorktree:
// `gl/<N>` optionally followed by `-<slug>`. The capture is the issue number.
var glBranchRE = regexp.MustCompile(`^gl/(\d+)(?:-.*)?$`)

// ticketNumberFromRef pulls the trailing #N out of a github:owner/repo#N ref.
// Returns 0 if the ref doesn't match.
func ticketNumberFromRef(ticket string) int {
	_, _, n, ok := parseTicketRef(ticket)
	if !ok {
		return 0
	}
	return n
}

// issueNumberFromBranch returns the issue number encoded in a `gl/<N>...`
// branch name, or 0 if the branch is not one of ours.
func issueNumberFromBranch(branch string) int {
	m := glBranchRE.FindStringSubmatch(branch)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// handleOpenPR dispatches an open_pr control frame. Runs the actual work in
// a goroutine because it needs to round-trip to the server for the GitHub
// token (SendRequest) — running synchronously would deadlock the read loop.
// See tickets.go for the same pattern.
func (d *DaemonWS) handleOpenPR(data []byte) {
	var msg struct {
		RelayID   string `json:"relay_id"`
		RequestID string `json:"request_id"`
		Title     string `json:"title"`
		Body      string `json:"body"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RelayID == "" || msg.RequestID == "" {
		log.Printf("daemon-ws: open_pr missing relay_id/request_id")
		return
	}
	go d.serveOpenPR(msg.RelayID, msg.RequestID, msg.Title, msg.Body)
}

func (d *DaemonWS) serveOpenPR(relayID, requestID, title, body string) {
	d.mu.RLock()
	sw := d.sessions[relayID]
	d.mu.RUnlock()
	if sw == nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", "unknown session")
		return
	}

	cwd := sw.cwd
	owner, repo, err := repoFromCwd(cwd)
	if err != nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", fmt.Sprintf("repo: %v", err))
		return
	}

	branch := branchForWorktree(cwd)
	if branch == "" {
		d.sendOpenPRResult(relayID, requestID, 0, "", "could not resolve branch")
		return
	}

	// Confirm the branch corresponds to an issue (either via `gl/<N>...`
	// or via the session's --ticket). Either signal is enough.
	issueN := issueNumberFromBranch(branch)
	if issueN == 0 {
		issueN = ticketNumberFromRef(sw.ticket)
	}
	if issueN == 0 {
		d.sendOpenPRResult(relayID, requestID, 0, "", "not_a_ticket_session")
		return
	}

	baseBranch := defaultBranchFromRemote(cwd)

	// Refuse to open a PR with no commits or a dirty tree — the phone UX
	// surfaces these as actionable errors.
	if n, err := commitCount(cwd, "origin/"+baseBranch, "HEAD"); err != nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", fmt.Sprintf("rev-list: %v", err))
		return
	} else if n == 0 {
		d.sendOpenPRResult(relayID, requestID, 0, "", "no_commits")
		return
	}
	if dirty, err := workingTreeDirty(cwd); err != nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", fmt.Sprintf("status: %v", err))
		return
	} else if dirty {
		d.sendOpenPRResult(relayID, requestID, 0, "", "dirty_worktree")
		return
	}

	push := exec.Command("git", "-C", cwd, "push", "-u", "origin", branch)
	if out, err := push.CombinedOutput(); err != nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", fmt.Sprintf("push_failed: %s", strings.TrimSpace(string(out))))
		return
	}

	token, err := d.fetchGitHubToken()
	if err != nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", fmt.Sprintf("token: %v", err))
		return
	}

	number, htmlURL, err := createGitHubPR(string(token), owner, repo, title, body, branch, baseBranch)
	if err != nil {
		d.sendOpenPRResult(relayID, requestID, 0, "", err.Error())
		return
	}
	d.sendOpenPRResult(relayID, requestID, number, htmlURL, "")
}

func (d *DaemonWS) sendOpenPRResult(relayID, requestID string, prNumber int, prURL, errMsg string) {
	resp := map[string]interface{}{
		"type":       "open_pr_result",
		"relay_id":   relayID,
		"request_id": requestID,
		"success":    errMsg == "",
		"pr_number":  prNumber,
		"pr_url":     prURL,
		"error":      errMsg,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: marshal open_pr_result: %v", err)
		return
	}
	d.ws.SendText(out)
}

// commitCount returns the number of commits in <base>..<head> in cwd.
func commitCount(cwd, base, head string) (int, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-list", "--count", base+".."+head)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// workingTreeDirty reports whether `git status --porcelain` has any output.
func workingTreeDirty(cwd string) (bool, error) {
	cmd := exec.Command("git", "-C", cwd, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// createGitHubPR posts to /repos/{owner}/{repo}/pulls and returns the new
// PR's number and html_url.
func createGitHubPR(token, owner, repo, title, body, head, base string) (int, string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls",
		strings.TrimRight(githubAPIBase(), "/"),
		url.PathEscape(owner), url.PathEscape(repo))
	payload := map[string]string{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
	}
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return 0, "", fmt.Errorf("github %d: %s", resp.StatusCode, githubErrorMessage(raw))
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, "", fmt.Errorf("parse pr response: %w", err)
	}
	return out.Number, out.HTMLURL, nil
}

// githubErrorMessage extracts the human-readable message from a GitHub
// error response, falling back to the raw body if the JSON shape is
// unexpected. GitHub puts the actionable detail in `.message`.
func githubErrorMessage(body []byte) string {
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &msg); err == nil && msg.Message != "" {
		return msg.Message
	}
	return strings.TrimSpace(string(body))
}

// ----------------------------------------------------------------------------
// merge_pr — device-scoped (no live session required).

// handleMergePR dispatches a merge_pr control frame. Device-scoped: server
// allowlists this with relay_id="", same shape as secrets_get.
func (d *DaemonWS) handleMergePR(data []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Owner     string `json:"owner"`
		Repo      string `json:"repo"`
		Number    int    `json:"number"`
		Method    string `json:"method"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.RequestID == "" || msg.Owner == "" || msg.Repo == "" || msg.Number == 0 {
		log.Printf("daemon-ws: merge_pr missing required fields")
		return
	}
	method := msg.Method
	switch method {
	case "merge", "squash", "rebase":
	case "":
		method = "squash"
	default:
		d.sendMergePRResult(msg.RequestID, "", fmt.Sprintf("invalid merge method %q", method))
		return
	}
	go d.serveMergePR(msg.RequestID, msg.Owner, msg.Repo, msg.Number, method)
}

func (d *DaemonWS) serveMergePR(requestID, owner, repo string, number int, method string) {
	token, err := d.fetchGitHubToken()
	if err != nil {
		d.sendMergePRResult(requestID, "", fmt.Sprintf("token: %v", err))
		return
	}
	sha, err := mergeGitHubPR(string(token), owner, repo, number, method)
	if err != nil {
		d.sendMergePRResult(requestID, "", err.Error())
		return
	}
	d.sendMergePRResult(requestID, sha, "")
}

func (d *DaemonWS) sendMergePRResult(requestID, sha, errMsg string) {
	resp := map[string]interface{}{
		"type":       "merge_pr_result",
		"relay_id":   "",
		"request_id": requestID,
		"success":    errMsg == "",
		"sha":        sha,
		"error":      errMsg,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		log.Printf("daemon-ws: marshal merge_pr_result: %v", err)
		return
	}
	d.ws.SendText(out)
}

// mergeGitHubPR calls PUT /repos/{owner}/{repo}/pulls/{number}/merge.
// On 405 (non-mergeable) the GitHub message is surfaced in the error so the
// phone can show it ("Branch is not mergeable", etc.).
func mergeGitHubPR(token, owner, repo string, number int, method string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge",
		strings.TrimRight(githubAPIBase(), "/"),
		url.PathEscape(owner), url.PathEscape(repo), number)
	payload := map[string]string{"merge_method": method}
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("PUT", u, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("github %d: %s", resp.StatusCode, githubErrorMessage(raw))
	}
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse merge response: %w", err)
	}
	if !out.Merged {
		return out.SHA, fmt.Errorf("github reported merge=false")
	}
	return out.SHA, nil
}
