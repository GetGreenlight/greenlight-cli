//go:build darwin || linux

package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// markSessionAwaitingUser tells the local daemon that this session has handed
// control back to a human (a ticket handoff: submit/approve/reject/merge/close).
// The daemon relays a session_await_user to the server, which derives "waiting"
// and suppresses the idle push until the user re-engages.
//
// Best-effort and non-fatal: a no-op when not running inside a session
// (GREENLIGHT_SESSION_ID unset), and it never fails the command — the stage move
// / merge that triggered it already happened, so a daemon hiccup must not turn a
// successful handoff into a non-zero exit. Errors go to the log, not stderr.
func markSessionAwaitingUser() {
	relayID := os.Getenv("GREENLIGHT_SESSION_ID")
	if relayID == "" {
		return
	}
	if _, err := ipcExchange(ipcRequest{Type: "await_user", RelayID: relayID}); err != nil {
		log.Printf("ticket: await_user signal failed (non-fatal): %v", err)
	}
}

// runTicket implements `greenlight ticket <list|show|create|update|close|reopen>`.
// It is the agent-facing companion to the phone's ticket views: the same
// provider abstraction and config resolution back both paths. The repo is
// resolved from the current working directory's `origin` remote; the provider
// and API-token secret come from config (`tickets_provider` / `tickets_secret`,
// project override → host).
//
// Ticket data is printed to stdout (so an agent can consume it); status lines
// and errors go to stderr (see the cli/CLAUDE.md "never print to stdout"
// convention — stdout carries the payload, nothing else).
func runTicket(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printTicketUsage()
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}

	action := args[0]
	rest := args[1:]

	project := os.Getenv("GREENLIGHT_PROJECT")
	var title, body, state, setCSV *string
	var clearFlag bool
	var prFlag, mergeMethod, branchFlag string
	var positional []string

	needVal := func(i int) string {
		if i+1 >= len(rest) {
			fmt.Fprintf(os.Stderr, "greenlight: %s requires a value\n", rest[i])
			os.Exit(1)
		}
		return rest[i+1]
	}

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--project", "-p":
			project = needVal(i)
			i++
		case "--title", "-t":
			v := needVal(i)
			title = &v
			i++
		case "--body", "-b":
			v := needVal(i)
			body = &v
			i++
		case "--state", "-s":
			v := needVal(i)
			state = &v
			i++
		case "--set":
			v := needVal(i)
			setCSV = &v
			i++
		case "--clear":
			clearFlag = true
		case "--pr":
			prFlag = needVal(i)
			i++
		case "--method":
			mergeMethod = needVal(i)
			i++
		case "--branch":
			branchFlag = needVal(i)
			i++
		default:
			positional = append(positional, rest[i])
		}
	}

	if project == "" {
		fmt.Fprintln(os.Stderr, "greenlight: project required (pass --project P or set GREENLIGHT_PROJECT)")
		os.Exit(1)
	}
	cwd, _ := os.Getwd()

	switch action {
	case "list", "ls":
		env := mustTicketEnv(project, cwd)
		tickets, err := env.provider.List(env.owner, env.repo, env.token)
		if err != nil {
			ticketFail(err.Error())
		}
		for _, t := range tickets {
			fmt.Printf("#%s\t%s\t%s\t%s\n", t.OpaqueID, t.DisplayLabel, t.Title, t.URL)
		}
		fmt.Fprintf(os.Stderr, "%d ticket(s) in %s/%s\n", len(tickets), env.owner, env.repo)

	case "show", "read", "get":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight ticket show <id> [--project P]")
			os.Exit(1)
		}
		env := mustTicketEnv(project, cwd)
		detail, err := env.provider.Read(env.owner, env.repo, env.token, positional[0])
		if err != nil {
			ticketFail(err.Error())
		}
		printTicketDetail(detail)

	case "create", "new":
		if title == nil || strings.TrimSpace(*title) == "" {
			fmt.Fprintln(os.Stderr, "Usage: greenlight ticket create --title T [--body B] [--project P]")
			os.Exit(1)
		}
		in := TicketInput{Title: *title}
		if body != nil {
			in.Body = *body
		}
		env := mustTicketEnv(project, cwd)
		detail, err := env.provider.Create(env.owner, env.repo, env.token, in)
		if err != nil {
			ticketFail(err.Error())
		}
		fmt.Printf("#%s\t%s\t%s\t%s\n", detail.OpaqueID, detail.DisplayLabel, detail.Title, detail.URL)
		fmt.Fprintf(os.Stderr, "created #%s in %s/%s\n", detail.OpaqueID, env.owner, env.repo)

	case "update", "edit":
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: greenlight ticket update <id> [--title T] [--body B] [--state open|closed] [--project P]")
			os.Exit(1)
		}
		if title == nil && body == nil && state == nil {
			fmt.Fprintln(os.Stderr, "greenlight: nothing to update (pass --title, --body, and/or --state)")
			os.Exit(1)
		}
		if state != nil && *state != "open" && *state != "closed" {
			fmt.Fprintln(os.Stderr, "greenlight: --state must be open or closed")
			os.Exit(1)
		}
		env := mustTicketEnv(project, cwd)
		detail, err := env.provider.Update(env.owner, env.repo, env.token, positional[0], TicketPatch{Title: title, Body: body, State: state})
		if err != nil {
			ticketFail(err.Error())
		}
		fmt.Printf("#%s\t%s\t%s\t%s\n", detail.OpaqueID, detail.DisplayLabel, detail.Title, detail.URL)
		fmt.Fprintf(os.Stderr, "updated #%s (%s)\n", detail.OpaqueID, detail.DisplayLabel)

	case "close", "reopen":
		if len(positional) != 1 {
			fmt.Fprintf(os.Stderr, "Usage: greenlight ticket %s <id> [--project P]\n", action)
			os.Exit(1)
		}
		newState := "closed"
		if action == "reopen" {
			newState = "open"
		}
		env := mustTicketEnv(project, cwd)
		detail, err := env.provider.Update(env.owner, env.repo, env.token, positional[0], TicketPatch{State: &newState})
		if err != nil {
			ticketFail(err.Error())
		}
		fmt.Printf("#%s\t%s\t%s\t%s\n", detail.OpaqueID, detail.DisplayLabel, detail.Title, detail.URL)
		fmt.Fprintf(os.Stderr, "%sd #%s\n", action, detail.OpaqueID)
		if action == "close" {
			// Closing is a handoff back to the human (reopen is not).
			markSessionAwaitingUser()
		}

	case "merge":
		runTicketMerge(project, cwd, positional, prFlag, mergeMethod, branchFlag)

	case "stage":
		runTicketStage(project, cwd, positional, clearFlag)

	case "start", "submit", "approve", "reject":
		runTicketStageMove(project, cwd, positional, action)

	case "tag", "tags":
		runTicketTag(project, cwd, positional, setCSV, clearFlag)

	default:
		fmt.Fprintf(os.Stderr, "greenlight: unknown ticket action %q\n", action)
		printTicketUsage()
		os.Exit(1)
	}
}

// runTicketMerge implements `greenlight ticket merge`, dispatching on the
// resolved provider: the github provider merges the linked PR via the API; the
// built-in greenlight provider does a regular local git merge (no PR, no token).
// It resolves the ticket id (positional → in-scope) and validates the flags
// first. stdout leads with the artifact acted on (the PR for github, the ticket
// for greenlight); the stderr status reports the resulting state.
func runTicketMerge(project, cwd string, positional []string, prFlag, method, branch string) {
	id := ""
	if len(positional) >= 1 {
		id = positional[0]
	}
	if id == "" {
		id = inScopeTicketID()
	}

	prNum := 0
	if prFlag != "" {
		n, err := strconv.Atoi(prFlag)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "greenlight: --pr must be a positive number")
			os.Exit(1)
		}
		prNum = n
	}

	if method != "" && method != "merge" && method != "squash" && method != "rebase" {
		ticketFail("invalid_merge_method")
	}

	env := mustTicketEnv(project, cwd)

	// Built-in tickets have no PR — merge is a local git merge dispatched here,
	// before any provider method is called.
	if env.providerName == builtinTicketsProvider {
		runTicketMergeGreenlight(env, project, cwd, id, method, branch)
		return
	}

	if id == "" && prNum == 0 {
		fmt.Fprintln(os.Stderr, "Usage: greenlight ticket merge [<id>] [--pr <n>] [--method merge|squash|rebase] [--project P]")
		os.Exit(1)
	}

	res, err := env.provider.Merge(env.owner, env.repo, env.token, id, MergeOptions{PR: prNum, Method: method})
	if err != nil {
		ticketFail(err.Error())
	}

	// stdout payload: PR-led, deliberately distinct from the ticket-led shape of
	// the other verbs (see docs/ticket-merge-spec.md §3.1).
	fmt.Printf("#%d\t%s\t%s\t%s\n", res.PR, "merged", res.Title, res.URL)

	methodLabel := method
	if methodLabel == "" {
		methodLabel = "merge"
	}
	switch {
	case res.AlreadyMerged && id != "" && res.IssueClosed:
		fmt.Fprintf(os.Stderr, "PR #%d already merged; ticket #%s closed\n", res.PR, id)
	case res.AlreadyMerged:
		fmt.Fprintf(os.Stderr, "PR #%d already merged\n", res.PR)
	case id != "" && res.IssueClosed:
		fmt.Fprintf(os.Stderr, "merged PR #%d (%s); ticket #%s auto-closed\n", res.PR, methodLabel, id)
	case id != "":
		fmt.Fprintf(os.Stderr, "merged PR #%d (%s); ticket #%s still open — add \"Closes #%s\" to the PR or run `greenlight ticket close %s`\n", res.PR, methodLabel, id, id, id)
	default:
		fmt.Fprintf(os.Stderr, "merged PR #%d (%s)\n", res.PR, methodLabel)
	}

	// Merging is a handoff back to the human (the reviewer's "passes" finish step).
	markSessionAwaitingUser()

	// Signal the server to tag a reserved `done` on the merged ticket (#159).
	// Best-effort and non-fatal — the merge already happened.
	signalTicketMerged(project, cwd, id)
}

// runTicketMergeGreenlight is the built-in provider's `ticket merge`: a regular
// local git merge of the work branch into the repo's default branch, then a
// push, then flipping the ticket closed (#176 §6). No PR, no token. The merge is
// transactional — a conflict, a non-fast-forwardable default branch, or a
// rejected push aborts cleanly and leaves the ticket open (mergeGreenlightLocal
// hard-resets and restores the prior branch). On success it flips the ticket to
// closed, which the autopilot completion path detects server-side via the
// await-user probe, and signals the reserved `done` tag.
func runTicketMergeGreenlight(env *ticketEnv, project, cwd, id, method, branch string) {
	if id == "" {
		// Unlike github (which can merge a bare --pr), a built-in merge must know
		// which ticket to close.
		fmt.Fprintln(os.Stderr, "Usage: greenlight ticket merge <id> [--method merge|squash] [--branch <name>] [--project P]")
		os.Exit(1)
	}
	if method == "rebase" {
		// Local merge supports a merge commit or a squash; rebase isn't a merge mode.
		ticketFail("invalid_merge_method")
	}

	work := branch
	if work == "" {
		b, err := gitCurrentBranch(cwd)
		if err != nil {
			ticketFail("no_repo")
		}
		work = b
	}
	base := gitDefaultBranch(cwd)

	res, err := mergeGreenlightLocal(cwd, work, base, method)
	if err != nil {
		ticketFail(err.Error())
	}

	// Land succeeded; flip the ticket closed. If this fails the branch is already
	// merged+pushed, so report honestly and exit non-zero rather than pretend.
	closed := "closed"
	detail, uerr := env.provider.Update(env.owner, env.repo, env.token, id, TicketPatch{State: &closed})
	if uerr != nil {
		fmt.Fprintf(os.Stderr, "greenlight: merged %s into %s but could not close ticket #%s (%s); run `greenlight ticket close %s`\n",
			res.WorkBranch, res.DefaultBranch, id, uerr.Error(), id)
		os.Exit(1)
	}

	// stdout payload: ticket-led (there is no PR for a built-in ticket).
	fmt.Printf("#%s\t%s\t%s\t%s\n", detail.OpaqueID, detail.DisplayLabel, detail.Title, detail.URL)
	methodLabel := "merge"
	if res.Squashed {
		methodLabel = "squash"
	}
	fmt.Fprintf(os.Stderr, "merged %s into %s (%s); ticket #%s closed\n", res.WorkBranch, res.DefaultBranch, methodLabel, id)

	// Merging is a handoff back to the human (the reviewer's "passes" finish step).
	markSessionAwaitingUser()

	// Signal the server to tag a reserved `done` on the merged ticket (#159).
	// Best-effort and non-fatal — the merge already happened.
	signalTicketMerged(project, cwd, id)
}

// signalTicketMerged tells the local daemon that a greenlight agent merged the
// given ticket, so the server can tag a reserved `done` (issue #159). It resolves
// the provider + canonical repo_key without a provider token (resolveTagTarget),
// then sends a best-effort, fire-and-forget IPC. A no-op when the ticket id or
// target can't be resolved (e.g. a `--pr`-only merge with no issue id), and it
// never fails the command — the merge already happened. Errors go to the log.
func signalTicketMerged(project, cwd, id string) {
	if id == "" {
		return
	}
	provider, repoKey, errCode := resolveTagTarget(project, cwd)
	if errCode != "" || provider == "" || repoKey == "" {
		return
	}
	if _, err := ipcExchange(ipcRequest{
		Type:     "ticket_merged",
		Provider: provider,
		RepoKey:  repoKey,
		OpaqueID: id,
	}); err != nil {
		log.Printf("ticket: ticket_merged signal failed (non-fatal): %v", err)
	}
}

// mustTicketEnv resolves the ticket env or exits with a human-readable error.
func mustTicketEnv(project, cwd string) *ticketEnv {
	env, _, errCode := resolveTicketEnv(project, cwd)
	if errCode != "" {
		ticketFail(errCode)
	}
	return env
}

// ticketFail prints a wire error code as a human message to stderr and exits.
func ticketFail(code string) {
	fmt.Fprintf(os.Stderr, "greenlight: %s\n", ticketErrorMessage(code))
	os.Exit(1)
}

// ticketErrorMessage maps a wire error code to a human-readable CLI message.
func ticketErrorMessage(code string) string {
	switch code {
	case "not_configured":
		return "tickets not configured (set tickets_provider and tickets_secret via `greenlight config set`)"
	case "unsupported provider":
		return fmt.Sprintf("unsupported tickets_provider (supported: %s)", joinSortedSet(knownTicketProviders))
	case "no_repo":
		return "could not resolve a repository from the project's origin remote"
	case "missing_token":
		return "could not read the provider API token (check tickets_secret and that the daemon is running)"
	case "repo_not_found":
		return "repository not found (or the token can't see it)"
	case "rate_limited":
		return "github API rate limit exceeded"
	case "not_a_ticket":
		return "that id refers to a pull request, not a ticket"
	case "invalid_tag":
		return "invalid tag (tags are lowercased to [a-z0-9._-], whitespace becomes '-', max 32 chars)"
	case "too_many_tags":
		return "too many tags (max 24 per ticket)"
	case "invalid_stage":
		return "invalid stage (must be one of the fixed workflow stages, e.g. spec-needed, code-in-progress)"
	case "no_linked_pr":
		return "no open PR references this ticket (add \"Closes #<id>\" to the PR body, or pass --pr <n>)"
	case "ambiguous_pr":
		return "multiple open PRs reference this ticket; pass --pr <n> to pick one"
	case "merge_conflict":
		return "merge conflict; resolve it before merging"
	case "not_mergeable":
		return "the PR is not in a mergeable state (failing checks or branch protection)"
	case "pr_closed":
		return "that PR is closed and can't be merged"
	case "dirty_tree":
		return "the working tree has uncommitted changes; commit or stash them before merging"
	case "on_default_branch":
		return "already on the default branch; check out the work branch you want to merge (or pass --branch)"
	case "not_ahead":
		return "the work branch has no commits to merge into the default branch"
	case "checkout_failed":
		return "could not check out the default branch"
	case "pull_failed":
		return "could not fast-forward the default branch from origin; reconcile it and retry"
	case "push_failed":
		return "the merge was undone because the push to origin was rejected (pull and retry, or check branch protection)"
	case "invalid_merge_method":
		return "--method must be merge, squash, or rebase"
	case "missing_id":
		return "missing ticket id"
	case "missing_repo_key", "missing_provider":
		return "could not resolve the ticket's provider/repository"
	case "write_error", "read_error":
		return "the server could not access the tag/stage store"
	default:
		return code
	}
}

func printTicketDetail(d *TicketDetail) {
	fmt.Printf("#%s  %s\n", d.OpaqueID, d.Title)
	fmt.Printf("state: %s\n", d.DisplayLabel)
	if d.Author != "" {
		fmt.Printf("author: %s\n", d.Author)
	}
	fmt.Printf("url: %s\n", d.URL)
	if strings.TrimSpace(d.Body) != "" {
		fmt.Printf("\n%s\n", d.Body)
	}
}

func printTicketUsage() {
	fmt.Fprintf(os.Stderr, "Usage: greenlight ticket <command> [args] [--project P]\n\n")
	fmt.Fprintf(os.Stderr, "  ticket list                                       list open + closed tickets\n")
	fmt.Fprintf(os.Stderr, "  ticket show <id>                                  show a ticket's detail\n")
	fmt.Fprintf(os.Stderr, "  ticket create --title T [--body B]                create a ticket\n")
	fmt.Fprintf(os.Stderr, "  ticket update <id> [--title T] [--body B] [--state open|closed]\n")
	fmt.Fprintf(os.Stderr, "  ticket close <id>                                 close a ticket (no merge)\n")
	fmt.Fprintf(os.Stderr, "  ticket reopen <id>                                reopen a ticket\n")
	fmt.Fprintf(os.Stderr, "  ticket merge [<id>] [--pr <n>] [--method merge|squash|rebase] [--branch <name>]\n")
	fmt.Fprintf(os.Stderr, "                                                    github: merge the linked PR (Closes #N);\n")
	fmt.Fprintf(os.Stderr, "                                                    greenlight: local git merge of the work branch\n")
	fmt.Fprintf(os.Stderr, "  ticket stage <id> [<value> | --clear]             get/set a ticket's workflow stage\n")
	fmt.Fprintf(os.Stderr, "  ticket start [<id>]                               begin work: move stage to *-in-progress\n")
	fmt.Fprintf(os.Stderr, "  ticket submit [<id>]                              ready for review: move stage to *-in-review\n")
	fmt.Fprintf(os.Stderr, "  ticket approve [<id>]                             passed review: spec-in-review → code-needed\n")
	fmt.Fprintf(os.Stderr, "  ticket reject [<id>]                              needs rework: *-in-review → *-in-progress\n")
	fmt.Fprintf(os.Stderr, "  ticket tag <id>                                   list a ticket's tags\n")
	fmt.Fprintf(os.Stderr, "  ticket tag <id> +foo -bar | --set a,b,c | --clear edit a ticket's tags\n\n")
	fmt.Fprintf(os.Stderr, "The provider comes from config (tickets_provider); it defaults to the built-in\n")
	fmt.Fprintf(os.Stderr, "\"greenlight\" provider (no token/OAuth). Set tickets_provider=github (with\n")
	fmt.Fprintf(os.Stderr, "tickets_secret) for GitHub-backed tickets.\n")
	fmt.Fprintf(os.Stderr, "The repository is resolved from the current directory's origin remote.\n")
	fmt.Fprintf(os.Stderr, "Project defaults to $GREENLIGHT_PROJECT; override with --project.\n")
}
