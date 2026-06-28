//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// runTicketStage implements `greenlight ticket stage <id> [<value>|--clear]`,
// the agent-facing reader/writer for a ticket's workflow stage. Unlike tags,
// a stage is a single scalar value per ticket (see docs/ticket-tags-spec.md §9).
// Tags and stages are both Greenlight-owned and server-stored, so this talks to
// the server over the daemon WS (via daemonWSRequest) and never touches the
// provider API.
//
// Forms:
//
//	ticket stage <id>            print the current stage (empty line if none)
//	ticket stage <id> <value>    set the stage (single value, replace)
//	ticket stage <id> --clear    clear the stage
func runTicketStage(project, cwd string, positional []string, clear bool) {
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: greenlight ticket stage <id> [<value> | --clear]")
		os.Exit(1)
	}
	id := positional[0]
	rest := positional[1:]
	if clear && len(rest) > 0 {
		fmt.Fprintln(os.Stderr, "greenlight: choose a stage value or --clear, not both")
		os.Exit(1)
	}
	if len(rest) > 1 {
		fmt.Fprintln(os.Stderr, "greenlight: a ticket has a single stage; pass one value")
		os.Exit(1)
	}

	provider, repoKey, errCode := resolveTagTarget(project, cwd)
	if errCode != "" {
		ticketFail(errCode)
	}

	var stage string
	switch {
	case clear:
		stage = ticketStageSet(provider, repoKey, id, "")
	case len(rest) == 1:
		stage = ticketStageSet(provider, repoKey, id, rest[0])
	default:
		stage = ticketStageGet(provider, repoKey, id)
	}

	if stage != "" {
		fmt.Println(stage)
		fmt.Fprintf(os.Stderr, "stage of #%s: %s\n", id, stage)
	} else {
		fmt.Fprintf(os.Stderr, "no stage on #%s\n", id)
	}
}

// runTicketStageMove implements the agent-facing `greenlight ticket
// start|submit|approve|reject [id]` — idempotent shortcuts over the stage scalar
// so a prompt can say "...when done, run `greenlight ticket submit`" without
// teaching the agent the stage vocabulary. The verbs split by role:
//
//	start    (author)   move a *-needed ticket to *-in-progress
//	submit   (author)   move a *-needed/*-in-progress ticket to *-in-review
//	approve  (reviewer) advance past the review gate: spec-in-review → code-needed
//	reject   (reviewer) send back for rework: *-in-review → *-in-progress
//
// Each verb only acts on the stages it makes sense for; on any other stage it's
// a no-op. The target is derived from the current stage, so a retry just
// re-applies the same value (no double-advance). approve at code-in-review is a
// no-op — that's the final review stage, so close the provider ticket with
// `greenlight ticket close` when the work is done (we never auto-close). The id
// defaults to the session's in-scope ticket (GREENLIGHT_TICKET_JSON), so an
// agent run inside a ticket session can omit it entirely.
func runTicketStageMove(project, cwd string, positional []string, verb string) {
	var id string
	if len(positional) >= 1 {
		id = positional[0]
	} else {
		id = inScopeTicketID()
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight ticket %s [<id>]  (id defaults to this session's ticket)\n", verb)
		os.Exit(1)
	}

	provider, repoKey, errCode := resolveTagTarget(project, cwd)
	if errCode != "" {
		ticketFail(errCode)
	}

	cur := ticketStageGet(provider, repoKey, id)
	var target string
	switch verb {
	case "start":
		target = stageStart(cur)
	case "submit":
		target = stageSubmit(cur)
	case "approve":
		target = stageApprove(cur)
	default: // reject
		target = stageReject(cur)
	}

	stage := cur
	if target != cur {
		stage = ticketStageSet(provider, repoKey, id, target)

		// Record review bounces as a Greenlight-owned tag so the author's
		// in-progress launch chip can switch to an "address the feedback"
		// prompt (see docs/ticket-tags-spec.md §9). A reject from *-in-review
		// sets "<phase>-rejected"; the matching submit (the only re-submit is
		// from *-in-progress) clears it so a clean re-review isn't flagged
		// stale. needed→in-review first submits never set the tag, so they
		// skip the get/set round-trip entirely. The tag write runs after the
		// stage set, which is the primary effect — if the tag wire op fails it
		// os.Exit(1)s here, leaving the stage already advanced (the acceptable
		// failure ordering).
		switch {
		case verb == "reject":
			phase, _ := splitStage(cur) // cur is "<phase>-in-review"
			merged := applyTagDeltas(ticketTagsGet(provider, repoKey, id),
				[]string{"+" + phase + "-rejected"})
			ticketTagsSet(provider, repoKey, id, merged)
		case verb == "submit":
			// Only the in-progress re-submit can be clearing a bounce; and
			// only actually write when the tag is present, so a clean submit
			// (work that was never rejected) skips the set entirely — no extra
			// round-trip and no spurious os.Exit(1) on a tag-wire hiccup.
			if phase, step := splitStage(cur); step == "in-progress" {
				rej := phase + "-rejected"
				tags := ticketTagsGet(provider, repoKey, id)
				if slices.Contains(tags, rej) {
					ticketTagsSet(provider, repoKey, id,
						applyTagDeltas(tags, []string{"-" + rej}))
				}
			}
		}
	}

	if stage != "" {
		fmt.Println(stage)
	}
	switch {
	case target != cur:
		fmt.Fprintf(os.Stderr, "#%s → %s\n", id, displayStage(stage))
	case verb == "approve" && cur == "code-in-review":
		fmt.Fprintf(os.Stderr, "#%s is at code-in-review (final stage); close it with `greenlight ticket close %s` when done\n", id, id)
	default:
		fmt.Fprintf(os.Stderr, "#%s already at stage %s\n", id, displayStage(cur))
	}

	// submit/approve/reject hand control back to a human (review or rework);
	// start is the author *beginning* work, so a stall after it should still
	// alert as idle — don't mark it.
	if verb != "start" {
		markSessionAwaitingUser()
	}
}

// stageStart returns the stage a `start` should move `cur` to: a *-needed stage
// advances to *-in-progress; anything else (already in progress, in review) is
// left unchanged. An empty stage is treated as the first stage (spec-needed).
func stageStart(cur string) string {
	phase, step := splitStage(cur)
	if step == "needed" {
		return phase + "-in-progress"
	}
	return cur
}

// stageSubmit returns the stage a `submit` should move `cur` to: a *-needed or
// *-in-progress stage advances to *-in-review; an already-in-review stage is
// left unchanged so the review gate isn't crossed. Empty → spec-needed.
func stageSubmit(cur string) string {
	phase, step := splitStage(cur)
	if step == "needed" || step == "in-progress" {
		return phase + "-in-review"
	}
	return cur
}

// stageApprove returns the stage an `approve` (reviewer accepts) should move
// `cur` to: spec-in-review advances past the gate to code-needed. code-in-review
// is the final review stage — there's no further stage, so it's left unchanged
// (the work is "done" when the provider ticket is closed via `greenlight ticket
// close`; we don't auto-close). Only acts on a *-in-review stage.
func stageApprove(cur string) string {
	phase, step := splitStage(cur)
	if step != "in-review" {
		return cur
	}
	if phase == "spec" {
		return "code-needed"
	}
	return cur // code-in-review: final review stage
}

// stageReject returns the stage a `reject` (reviewer returns for rework) should
// move `cur` to: a *-in-review stage goes back to *-in-progress. Anything else
// is left unchanged.
func stageReject(cur string) string {
	phase, step := splitStage(cur)
	if step == "in-review" {
		return phase + "-in-progress"
	}
	return cur
}

// splitStage splits a "<phase>-<step>" stage (e.g. "code-in-progress") into its
// phase ("spec"/"code") and step ("needed"/"in-progress"/"in-review"). An empty
// stage means a ticket with no stage yet, which behaves as the first stage.
func splitStage(s string) (phase, step string) {
	if s == "" {
		return "spec", "needed"
	}
	i := strings.Index(s, "-")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func displayStage(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// inScopeTicketID returns the opaque id of the session's in-scope ticket from
// GREENLIGHT_TICKET_JSON, or "" if no ticket is in scope / it can't be parsed.
func inScopeTicketID() string {
	blob := os.Getenv("GREENLIGHT_TICKET_JSON")
	if blob == "" {
		return ""
	}
	var t TicketRef
	if err := json.Unmarshal([]byte(blob), &t); err == nil {
		return t.OpaqueID
	}
	return ""
}

// ticketStageGet fetches the stored stage for a ticket over the daemon WS.
func ticketStageGet(provider, repoKey, id string) string {
	raw, err := daemonWSRequest("ticket_stage_get", map[string]interface{}{
		"provider":  provider,
		"repo_key":  repoKey,
		"opaque_id": id,
	}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	return decodeStageReply(raw)
}

// ticketStageSet sets (or clears, with an empty value) the stage for a ticket
// over the daemon WS and returns the normalized, stored stage.
func ticketStageSet(provider, repoKey, id, stage string) string {
	raw, err := daemonWSRequest("ticket_stage_set", map[string]interface{}{
		"provider":  provider,
		"repo_key":  repoKey,
		"opaque_id": id,
		"stage":     stage,
	}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	return decodeStageReply(raw)
}

// decodeStageReply parses a ticket_stage_*_response, exiting on a wire error.
func decodeStageReply(raw json.RawMessage) string {
	var resp struct {
		Stage string `json:"stage"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: invalid server response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		ticketFail(resp.Error)
	}
	return resp.Stage
}
