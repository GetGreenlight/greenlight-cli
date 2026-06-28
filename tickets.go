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

// TicketRef is the provider-agnostic reference to an issue/ticket. Mirrors
// the server-side struct; widened from the bare-string form at CLI v2.6.
type TicketRef struct {
	Provider string `json:"provider"`
	OpaqueID string `json:"opaque_id"`
	URL      string `json:"url"`
}

// TicketSummary is what the UI consumes for the Tickets list.
type TicketSummary struct {
	OpaqueID     string `json:"opaque_id"`
	Title        string `json:"title"`
	DisplayLabel string `json:"display_label"` // raw provider state (e.g. "open")
	CoarseState  string `json:"coarse_state"`  // reduced: "open" or "closed"
	URL          string `json:"url"`
}

// TicketDetail is the full view of a single ticket, returned by Read/Create/
// Update so the UI can render a detail screen and reflect edits.
type TicketDetail struct {
	OpaqueID     string `json:"opaque_id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	DisplayLabel string `json:"display_label"`
	CoarseState  string `json:"coarse_state"`
	URL          string `json:"url"`
	Author       string `json:"author,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// TicketInput carries the fields for creating a ticket.
type TicketInput struct {
	Title string
	Body  string
}

// TicketPatch carries the fields for updating a ticket. Nil pointers are left
// untouched; non-nil values are written. State is the coarse "open"/"closed".
type TicketPatch struct {
	Title *string
	Body  *string
	State *string
}

// MergeOptions carries the inputs for merging a ticket's PR.
type MergeOptions struct {
	PR     int    // explicit PR number; 0 = auto-resolve from the issue
	Method string // "merge" | "squash" | "rebase"; "" = merge
}

// MergeResult describes a completed (or already-completed) merge.
type MergeResult struct {
	PR            int    // the PR that was merged
	SHA           string // merge commit sha
	URL           string // PR html_url
	Title         string // PR title (for the output line)
	IssueClosed   bool   // whether the linked issue is now closed
	AlreadyMerged bool   // true when the PR was merged before this call (idempotent no-op)
}

// TicketProvider abstracts a ticket backend. Only github is implemented today;
// adding a provider means implementing this interface and registering it in
// providerFor (and adding its name to knownTicketProviders so it's config-
// settable). owner/repo identify the repository; token is the provider API
// token resolved from a greenlight secret.
type TicketProvider interface {
	List(owner, repo, token string) ([]TicketSummary, error)
	Read(owner, repo, token, id string) (*TicketDetail, error)
	Create(owner, repo, token string, in TicketInput) (*TicketDetail, error)
	Update(owner, repo, token, id string, patch TicketPatch) (*TicketDetail, error)
	Merge(owner, repo, token, id string, opts MergeOptions) (*MergeResult, error)
}

// providerFor returns the implementation for a provider name, or false.
func providerFor(name string) (TicketProvider, bool) {
	switch name {
	case "github":
		return githubProvider{}, true
	case "greenlight":
		return greenlightProvider{}, true
	}
	return nil, false
}

// builtinTicketsProvider is the built-in (Greenlight-owned) provider name and
// the default when `tickets_provider` is unset (issue #176).
const builtinTicketsProvider = "greenlight"

// providerNeedsToken reports whether a provider authenticates against an
// external API with a token resolved from a greenlight secret. The built-in
// "greenlight" provider stores tickets on permit-cloud and needs none, so
// resolveTicketEnv skips the tickets_secret lookup for it.
func providerNeedsToken(name string) bool {
	return name != builtinTicketsProvider
}

// repoCoordsFor resolves owner/repo for a provider from the project's cwd.
// github stays strict (github.com remotes only — the existing behavior); the
// built-in greenlight provider accepts any git host via the generic slug parser,
// so Greenlight-owned tickets work on GitLab/Bitbucket/self-hosted repos too.
func repoCoordsFor(provider, cwd string) (string, string, error) {
	if provider == builtinTicketsProvider {
		return gitRemoteSlug(cwd)
	}
	return gitRemoteOwnerRepo(cwd)
}

// maxTicketsPerState caps how many tickets we return from each state list.
// GitHub repos with many issues otherwise blow up the wire payload.
const maxTicketsPerState = 100

// githubAPIBase is the GitHub REST API root. A package var (not a const) only so
// tests can point the provider at an httptest server.
var githubAPIBase = "https://api.github.com"

// knownTicketProviders is the set of providers the CLI can fetch tickets for.
// It doubles as the validation allowlist for the `tickets_provider` config key
// (see validateConfigBatch). Only github is implemented today; adding a provider
// here also makes it config-settable.
var knownTicketProviders = map[string]bool{
	"github":     true,
	"greenlight": true,
}

// ticketEnv bundles the resolved provider, repo coordinates, and API token for
// a project — everything the CRUD handlers need to call the backend.
type ticketEnv struct {
	provider     TicketProvider
	providerName string
	owner        string
	repo         string
	token        string
}

// resolveTicketEnv resolves the configured provider, repository, and API token
// for a project. cwd is the directory whose `origin` git remote identifies the
// repo. Provider and secret are config-driven (project override → host). When
// `tickets_provider` is unset the provider defaults to the built-in "greenlight"
// backend (issue #176, decision #5): tickets work with just a git repo, no
// token/OAuth. Because this default is applied CLI-side, an older CLI never sees
// it (unset stays "tickets off") — so the flip is automatically version-scoped.
//
// On success it returns the env and an empty error code. On failure it returns
// a nil env, the provider name resolved so far (may be ""), and a wire error
// code: not_configured, unsupported provider, no_repo, missing_token.
func resolveTicketEnv(project, cwd string) (*ticketEnv, string, string) {
	providerName := resolveConfig(project, configKeyTicketsProvider)
	if providerName == "" {
		providerName = builtinTicketsProvider
	}
	prov, ok := providerFor(providerName)
	if !ok {
		return nil, providerName, "unsupported provider"
	}
	if cwd == "" {
		return nil, providerName, "no_repo"
	}
	owner, repo, err := repoCoordsFor(providerName, cwd)
	if err != nil {
		log.Printf("tickets: repo resolution failed for project %q cwd %q: %v", project, cwd, err)
		return nil, providerName, "no_repo"
	}
	// The built-in greenlight provider stores tickets on the server and needs no
	// API token; external providers (github) resolve one from the tickets_secret.
	token := ""
	if providerNeedsToken(providerName) {
		secretName := resolveConfig(project, configKeyTicketsSecret)
		if secretName == "" {
			return nil, providerName, "not_configured"
		}
		t, err := fetchAndDecrypt(secretName)
		if err != nil {
			log.Printf("tickets: %s: %v", secretName, err)
			return nil, providerName, "missing_token"
		}
		token = string(t)
	}
	return &ticketEnv{
		provider:     prov,
		providerName: providerName,
		owner:        owner,
		repo:         repo,
		token:        token,
	}, providerName, ""
}

// resolveTagTarget resolves the provider and canonical repo_key for the
// Greenlight-owned ticket-metadata ops (tags and stage). Unlike resolveTicketEnv
// it does NOT fetch the provider API token: this metadata is server-stored and
// never touches the provider API, so requiring a token (and failing with
// missing_token) would be wrong. Returns the provider, repo_key ("owner/repo"
// lowercased), and a wire error code (not_configured / unsupported provider /
// no_repo).
func resolveTagTarget(project, cwd string) (provider, repoKey, errCode string) {
	provider = resolveConfig(project, configKeyTicketsProvider)
	if provider == "" {
		// Same default-when-unset flip as resolveTicketEnv (issue #176): tags/stage
		// on a built-in ticket must key on the same provider the ticket stored under.
		provider = builtinTicketsProvider
	}
	if _, ok := providerFor(provider); !ok {
		return provider, "", "unsupported provider"
	}
	if cwd == "" {
		return provider, "", "no_repo"
	}
	// Use the provider-aware resolver so a greenlight ticket's tags/stage key on
	// the same repo_key the greenlight provider stored the ticket under (generic
	// slug for greenlight, strict github.com for github).
	owner, repo, err := repoCoordsFor(provider, cwd)
	if err != nil {
		log.Printf("tickets: repo resolution failed for project %q cwd %q: %v", project, cwd, err)
		return provider, "", "no_repo"
	}
	return provider, strings.ToLower(owner + "/" + repo), ""
}

// handleListTickets resolves the project's repo, fetches open + closed tickets
// via the configured provider, and replies with tickets_listed. Errors are
// returned as a non-empty `error` field with an empty tickets array — the UI
// renders these as banners.
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

	env, providerName, errCode := resolveTicketEnv(msg.Project, d.resolveProjectCwd(msg.Project))
	if errCode != "" {
		reply(providerName, "", "", nil, errCode)
		return
	}

	tickets, err := env.provider.List(env.owner, env.repo, env.token)
	if err != nil {
		log.Printf("daemon-ws: list_tickets: list failed: %v", err)
		reply(env.providerName, env.owner, env.repo, nil, err.Error())
		return
	}
	reply(env.providerName, env.owner, env.repo, tickets, "")
	log.Printf("daemon-ws: list_tickets: %s/%s → %d tickets (project=%q)", env.owner, env.repo, len(tickets), msg.Project)
}

// handleReadTicket fetches a single ticket's detail and replies ticket_read.
func (d *DaemonWS) handleReadTicket(data []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Project   string `json:"project"`
		OpaqueID  string `json:"opaque_id"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("daemon-ws: read_ticket: invalid JSON: %v", err)
		return
	}
	if msg.RequestID == "" {
		log.Printf("daemon-ws: read_ticket: missing request_id")
		return
	}

	// owner/repo are echoed so the server can compute the ticket's repo_key and
	// enrich the reply with Greenlight-owned tags (see ticket-tags-spec §4.3).
	reply := func(provider, owner, repo string, detail *TicketDetail, errMsg string) {
		resp := map[string]interface{}{
			"type":       "ticket_read",
			"request_id": msg.RequestID,
			"provider":   provider,
		}
		if owner != "" {
			resp["owner"] = owner
		}
		if repo != "" {
			resp["repo"] = repo
		}
		if detail != nil {
			resp["ticket"] = detail
		}
		if errMsg != "" {
			resp["error"] = errMsg
		}
		out, err := json.Marshal(resp)
		if err != nil {
			log.Printf("daemon-ws: read_ticket: marshal ticket_read: %v", err)
			return
		}
		d.ws.SendText(out)
	}

	if msg.Project == "" {
		reply("", "", "", nil, "missing project")
		return
	}
	if msg.OpaqueID == "" {
		reply("", "", "", nil, "missing_id")
		return
	}

	env, providerName, errCode := resolveTicketEnv(msg.Project, d.resolveProjectCwd(msg.Project))
	if errCode != "" {
		reply(providerName, "", "", nil, errCode)
		return
	}
	detail, err := env.provider.Read(env.owner, env.repo, env.token, msg.OpaqueID)
	if err != nil {
		log.Printf("daemon-ws: read_ticket: %s/%s #%s: %v", env.owner, env.repo, msg.OpaqueID, err)
		reply(env.providerName, env.owner, env.repo, nil, err.Error())
		return
	}
	reply(env.providerName, env.owner, env.repo, detail, "")
	log.Printf("daemon-ws: read_ticket: %s/%s #%s (project=%q)", env.owner, env.repo, msg.OpaqueID, msg.Project)
}

// handleCreateTicket creates a ticket and replies ticket_created with its detail.
func (d *DaemonWS) handleCreateTicket(data []byte) {
	var msg struct {
		RequestID string `json:"request_id"`
		Project   string `json:"project"`
		Title     string `json:"title"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("daemon-ws: create_ticket: invalid JSON: %v", err)
		return
	}
	if msg.RequestID == "" {
		log.Printf("daemon-ws: create_ticket: missing request_id")
		return
	}

	// owner/repo are echoed so the server can compute the ticket's repo_key and
	// enrich the reply with Greenlight-owned tags (see ticket-tags-spec §4.3).
	reply := func(provider, owner, repo string, detail *TicketDetail, errMsg string) {
		resp := map[string]interface{}{
			"type":       "ticket_created",
			"request_id": msg.RequestID,
			"provider":   provider,
		}
		if owner != "" {
			resp["owner"] = owner
		}
		if repo != "" {
			resp["repo"] = repo
		}
		if detail != nil {
			resp["ticket"] = detail
		}
		if errMsg != "" {
			resp["error"] = errMsg
		}
		out, err := json.Marshal(resp)
		if err != nil {
			log.Printf("daemon-ws: create_ticket: marshal ticket_created: %v", err)
			return
		}
		d.ws.SendText(out)
	}

	if msg.Project == "" {
		reply("", "", "", nil, "missing project")
		return
	}
	if strings.TrimSpace(msg.Title) == "" {
		reply("", "", "", nil, "missing_title")
		return
	}

	env, providerName, errCode := resolveTicketEnv(msg.Project, d.resolveProjectCwd(msg.Project))
	if errCode != "" {
		reply(providerName, "", "", nil, errCode)
		return
	}
	detail, err := env.provider.Create(env.owner, env.repo, env.token, TicketInput{Title: msg.Title, Body: msg.Body})
	if err != nil {
		log.Printf("daemon-ws: create_ticket: %s/%s: %v", env.owner, env.repo, err)
		reply(env.providerName, env.owner, env.repo, nil, err.Error())
		return
	}
	reply(env.providerName, env.owner, env.repo, detail, "")
	log.Printf("daemon-ws: create_ticket: %s/%s → #%s (project=%q)", env.owner, env.repo, detail.OpaqueID, msg.Project)
}

// handleUpdateTicket edits a ticket's title/body and/or state (open/closed) and
// replies ticket_updated with the refreshed detail. "Closing" a ticket is an
// update with state="closed".
func (d *DaemonWS) handleUpdateTicket(data []byte) {
	var msg struct {
		RequestID string  `json:"request_id"`
		Project   string  `json:"project"`
		OpaqueID  string  `json:"opaque_id"`
		Title     *string `json:"title"`
		Body      *string `json:"body"`
		State     *string `json:"state"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("daemon-ws: update_ticket: invalid JSON: %v", err)
		return
	}
	if msg.RequestID == "" {
		log.Printf("daemon-ws: update_ticket: missing request_id")
		return
	}

	// owner/repo are echoed so the server can compute the ticket's repo_key and
	// enrich the reply with Greenlight-owned tags (see ticket-tags-spec §4.3).
	reply := func(provider, owner, repo string, detail *TicketDetail, errMsg string) {
		resp := map[string]interface{}{
			"type":       "ticket_updated",
			"request_id": msg.RequestID,
			"provider":   provider,
		}
		if owner != "" {
			resp["owner"] = owner
		}
		if repo != "" {
			resp["repo"] = repo
		}
		if detail != nil {
			resp["ticket"] = detail
		}
		if errMsg != "" {
			resp["error"] = errMsg
		}
		out, err := json.Marshal(resp)
		if err != nil {
			log.Printf("daemon-ws: update_ticket: marshal ticket_updated: %v", err)
			return
		}
		d.ws.SendText(out)
	}

	if msg.Project == "" {
		reply("", "", "", nil, "missing project")
		return
	}
	if msg.OpaqueID == "" {
		reply("", "", "", nil, "missing_id")
		return
	}
	if msg.State != nil && *msg.State != "open" && *msg.State != "closed" {
		reply("", "", "", nil, "invalid_state")
		return
	}
	if msg.Title == nil && msg.Body == nil && msg.State == nil {
		reply("", "", "", nil, "no_changes")
		return
	}

	env, providerName, errCode := resolveTicketEnv(msg.Project, d.resolveProjectCwd(msg.Project))
	if errCode != "" {
		reply(providerName, "", "", nil, errCode)
		return
	}
	detail, err := env.provider.Update(env.owner, env.repo, env.token, msg.OpaqueID, TicketPatch{
		Title: msg.Title,
		Body:  msg.Body,
		State: msg.State,
	})
	if err != nil {
		log.Printf("daemon-ws: update_ticket: %s/%s #%s: %v", env.owner, env.repo, msg.OpaqueID, err)
		reply(env.providerName, env.owner, env.repo, nil, err.Error())
		return
	}
	reply(env.providerName, env.owner, env.repo, detail, "")
	log.Printf("daemon-ws: update_ticket: %s/%s #%s → %s (project=%q)", env.owner, env.repo, msg.OpaqueID, detail.DisplayLabel, msg.Project)
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

// ---- GitHub provider ----

type githubProvider struct{}

// githubIssue is the subset of the GitHub issue payload we consume.
type githubIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	User    *struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
}

func (githubProvider) List(owner, repo, token string) ([]TicketSummary, error) {
	open, err := fetchGitHubIssues(owner, repo, "open", token)
	if err != nil {
		return nil, err
	}
	closed, err := fetchGitHubIssues(owner, repo, "closed", token)
	if err != nil {
		return nil, err
	}
	return append(open, closed...), nil
}

func (githubProvider) Read(owner, repo, token, id string) (*TicketDetail, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/issues/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(id))
	issue, err := githubIssueRequest("GET", endpoint, token, nil)
	if err != nil {
		return nil, err
	}
	if issue.PullRequest != nil {
		// The issues endpoint also serves PRs; a ticket id pointing at a PR is
		// not a ticket as far as the UI is concerned.
		return nil, fmt.Errorf("not_a_ticket")
	}
	return issueToDetail(issue), nil
}

func (githubProvider) Create(owner, repo, token string, in TicketInput) (*TicketDetail, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/issues",
		url.PathEscape(owner), url.PathEscape(repo))
	payload := map[string]interface{}{"title": in.Title}
	if in.Body != "" {
		payload["body"] = in.Body
	}
	issue, err := githubIssueRequest("POST", endpoint, token, payload)
	if err != nil {
		return nil, err
	}
	return issueToDetail(issue), nil
}

func (githubProvider) Update(owner, repo, token, id string, patch TicketPatch) (*TicketDetail, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/issues/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(id))
	// GitHub serves and edits PRs through the same /issues/<n> endpoint, so
	// guard against editing or closing a pull request: GET first and reject PRs
	// before the PATCH ever mutates anything.
	if existing, err := githubIssueRequest("GET", endpoint, token, nil); err != nil {
		return nil, err
	} else if existing.PullRequest != nil {
		return nil, fmt.Errorf("not_a_ticket")
	}
	payload := map[string]interface{}{}
	if patch.Title != nil {
		payload["title"] = *patch.Title
	}
	if patch.Body != nil {
		payload["body"] = *patch.Body
	}
	if patch.State != nil {
		// GitHub's coarse state happens to match our "open"/"closed".
		payload["state"] = *patch.State
	}
	issue, err := githubIssueRequest("PATCH", endpoint, token, payload)
	if err != nil {
		return nil, err
	}
	return issueToDetail(issue), nil
}

// Merge resolves the PR linked to the issue (or the explicit --pr), guards
// against unmergeable states, and merges it. Merging a `Closes #N` PR auto-closes
// the linked issue, so this is the finish action for coded work — not `close`,
// which only flips the issue state without landing the branch.
func (githubProvider) Merge(owner, repo, token, id string, opts MergeOptions) (*MergeResult, error) {
	method := opts.Method
	if method == "" {
		method = "merge"
	}

	// Step 1 — resolve the PR.
	var pull *githubPull
	if opts.PR != 0 {
		p, err := githubGetPull(owner, repo, token, opts.PR)
		if err != nil {
			return nil, err
		}
		pull = p
	} else {
		pulls, err := githubListOpenPulls(owner, repo, token)
		if err != nil {
			return nil, err
		}
		var matches []githubPull
		for _, p := range pulls {
			if prBodyClosesIssue(p.Body, id) {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no_linked_pr")
		case 1:
			// The list endpoint omits merged/mergeable, so re-GET the single PR
			// for the pre-merge guard below.
			p, err := githubGetPull(owner, repo, token, matches[0].Number)
			if err != nil {
				return nil, err
			}
			pull = p
		default:
			return nil, fmt.Errorf("ambiguous_pr")
		}
	}

	// Step 2 — pre-merge guard.
	if pull.Merged {
		// Idempotent success: skip the PUT and report the existing merge. Mirrors
		// the no-op behavior of the other stage verbs (start/submit/approve/reject).
		return &MergeResult{
			PR:            pull.Number,
			SHA:           pull.MergeCommitSHA,
			URL:           pull.HTMLURL,
			Title:         pull.Title,
			IssueClosed:   issueIsClosed(owner, repo, token, id),
			AlreadyMerged: true,
		}, nil
	}
	if pull.State == "closed" {
		return nil, fmt.Errorf("pr_closed")
	}
	// GitHub computes `mergeable` asynchronously and returns null right after a
	// push; only block when it's a definite false. A `blocked` state is failing
	// checks / branch protection (not_mergeable); anything else is a conflict.
	if pull.Mergeable != nil && !*pull.Mergeable {
		if pull.MergeableState == "blocked" {
			return nil, fmt.Errorf("not_mergeable")
		}
		return nil, fmt.Errorf("merge_conflict")
	}

	// Step 3 — merge.
	mr, err := githubMergeRequest(owner, repo, token, pull.Number, method)
	if err != nil {
		return nil, err
	}

	// Step 4 — confirm issue closure (the merge only auto-closes if the PR body
	// had a closing keyword). We never close it ourselves; surfacing the gap is
	// more honest and matches the "we never auto-close" stance elsewhere.
	return &MergeResult{
		PR:          pull.Number,
		SHA:         mr.SHA,
		URL:         pull.HTMLURL,
		Title:       pull.Title,
		IssueClosed: issueIsClosed(owner, repo, token, id),
	}, nil
}

// issueIsClosed reports whether the issue with the given id is closed. A blank
// id (merge driven purely by --pr) or any read error is treated as "not known
// closed" — the caller degrades to the "still open" hint rather than failing.
func issueIsClosed(owner, repo, token, id string) bool {
	if id == "" {
		return false
	}
	d, err := githubProvider{}.Read(owner, repo, token, id)
	return err == nil && d.CoarseState == "closed"
}

// prBodyClosesIssue reports whether a PR body contains a GitHub closing keyword
// referencing issue `id` (e.g. "Closes #114"). Case-insensitive. The trailing
// `\b` stops "#114" from matching inside "#1140"; the leading `\b` keeps "fixes"
// from matching inside "prefixes".
func prBodyClosesIssue(body, id string) bool {
	if id == "" {
		return false
	}
	re := regexp.MustCompile(`(?i)\b(close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)\s+#` + regexp.QuoteMeta(id) + `\b`)
	return re.MatchString(body)
}

// githubPull is the subset of the GitHub pull-request payload we consume. The
// list endpoint populates body/number/title/html_url; merged/mergeable/
// merge_commit_sha are only filled by the single-PR GET.
type githubPull struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Body           string `json:"body"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// githubListOpenPulls fetches open PRs (capped at 100) so the auto-resolver can
// match each PR's body against the issue's closing keyword.
func githubListOpenPulls(owner, repo, token string) ([]githubPull, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/pulls?state=open&per_page=100",
		url.PathEscape(owner), url.PathEscape(repo))
	body, err := githubAPIGet(endpoint, token)
	if err != nil {
		return nil, err
	}
	var pulls []githubPull
	if err := json.Unmarshal(body, &pulls); err != nil {
		return nil, fmt.Errorf("decode pulls: %w", err)
	}
	return pulls, nil
}

// githubGetPull fetches a single PR (with merged/mergeable populated).
func githubGetPull(owner, repo, token string, pr int) (*githubPull, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/pulls/%d",
		url.PathEscape(owner), url.PathEscape(repo), pr)
	body, err := githubAPIGet(endpoint, token)
	if err != nil {
		return nil, err
	}
	var pull githubPull
	if err := json.Unmarshal(body, &pull); err != nil {
		return nil, fmt.Errorf("decode pull: %w", err)
	}
	return &pull, nil
}

// githubAPIGet performs a GET against the GitHub API and returns the raw body,
// mapping non-2xx via mapGitHubStatus.
func githubAPIGet(endpoint, token string) ([]byte, error) {
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
	if err := mapGitHubStatus(resp, body); err != nil {
		return nil, err
	}
	return body, nil
}

// githubMergeResponse decodes PUT /pulls/{n}/merge.
type githubMergeResponse struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// githubMergeRequest performs the merge PUT and decodes the result. The merge
// endpoint has two PR-specific failure codes that mapGitHubStatus doesn't know
// about — 405 (branch not in a mergeable state) and 409 (head moved / required
// checks) — so they're mapped here; everything else falls through.
func githubMergeRequest(owner, repo, token string, pr int, method string) (*githubMergeResponse, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/pulls/%d/merge",
		url.PathEscape(owner), url.PathEscape(repo), pr)
	payload, err := json.Marshal(map[string]string{"merge_method": method})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("PUT", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusMethodNotAllowed: // 405
		return nil, fmt.Errorf("not_mergeable")
	case http.StatusConflict: // 409
		return nil, fmt.Errorf("merge_conflict")
	}
	if err := mapGitHubStatus(resp, body); err != nil {
		return nil, err
	}
	var out githubMergeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode merge: %w", err)
	}
	return &out, nil
}

// issueToDetail converts a decoded GitHub issue to the wire TicketDetail.
func issueToDetail(it *githubIssue) *TicketDetail {
	author := ""
	if it.User != nil {
		author = it.User.Login
	}
	return &TicketDetail{
		OpaqueID:     strconv.Itoa(it.Number),
		Title:        it.Title,
		Body:         it.Body,
		DisplayLabel: it.State,
		CoarseState:  coarseGitHubState(it.State),
		URL:          it.HTMLURL,
		Author:       author,
		CreatedAt:    it.CreatedAt,
		UpdatedAt:    it.UpdatedAt,
	}
}

// githubIssueRequest performs a single-issue GitHub API call (GET/POST/PATCH)
// and decodes the response into a githubIssue. Status codes are mapped to the
// same wire error codes as fetchGitHubIssues so the UI handles them uniformly.
func githubIssueRequest(method, endpoint, token string, payload interface{}) (*githubIssue, error) {
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, endpoint, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if err := mapGitHubStatus(resp, body); err != nil {
		return nil, err
	}

	var issue githubIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("decode issue: %w", err)
	}
	return &issue, nil
}

// mapGitHubStatus translates a non-2xx GitHub response into a wire error code
// (rate_limited, missing_token, repo_not_found) or a generic error. Returns nil
// for 2xx.
func mapGitHubStatus(resp *http.Response, body []byte) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 403 with rate-limit headers means rate limited; otherwise token issue.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("rate_limited")
		}
		return fmt.Errorf("missing_token")
	case resp.StatusCode == http.StatusNotFound:
		// GitHub returns 404 for both "doesn't exist" and "private repo the
		// token can't see" — they're intentionally indistinguishable.
		return fmt.Errorf("repo_not_found")
	default:
		return fmt.Errorf("github api %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// fetchGitHubIssues calls the issues endpoint, filters out PRs, and converts
// to TicketSummary. Capped at maxTicketsPerState entries.
func fetchGitHubIssues(owner, repo, state, token string) ([]TicketSummary, error) {
	endpoint := fmt.Sprintf(githubAPIBase+"/repos/%s/%s/issues?state=%s&per_page=%d&sort=updated&direction=desc",
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
	if err := mapGitHubStatus(resp, body); err != nil {
		return nil, err
	}

	var raw []githubIssue
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
