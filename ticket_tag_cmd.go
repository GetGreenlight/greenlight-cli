//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// runTicketTag implements `greenlight ticket tag <id> [...]`, the agent-facing
// writer for Greenlight-owned ticket tags. Tags live in the server's table, so
// this talks to the server over the daemon WS (via daemonWSRequest) and does
// NOT touch the provider API — it's the generic, provider-independent way to
// attach freeform labels to a ticket (e.g. `+blocked -needs-spec`). A ticket's
// workflow stage is a separate scalar (`greenlight ticket stage`), not a tag.
// The server is replace-set only; incremental +/- is resolved client-side into
// a full set. See docs/ticket-tags-spec.md §4.
//
// Forms:
//
//	ticket tag <id>                  list current tags (one per line)
//	ticket tag <id> +foo +bar -baz   add foo,bar; remove baz (incremental)
//	ticket tag <id> --set a,b,c      replace the entire set
//	ticket tag <id> --clear          remove all tags
func runTicketTag(project, cwd string, positional []string, setCSV *string, clear bool) {
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: greenlight ticket tag <id> [+add -remove | --set a,b,c | --clear]")
		os.Exit(1)
	}
	id := positional[0]
	deltas := positional[1:]

	// The three write modes are mutually exclusive.
	modes := 0
	if clear {
		modes++
	}
	if setCSV != nil {
		modes++
	}
	if len(deltas) > 0 {
		modes++
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "greenlight: choose one of +/- deltas, --set, or --clear")
		os.Exit(1)
	}

	provider, repoKey, errCode := resolveTagTarget(project, cwd)
	if errCode != "" {
		ticketFail(errCode)
	}

	var stored []string
	switch {
	case clear:
		stored = ticketTagsSet(provider, repoKey, id, []string{})
	case setCSV != nil:
		stored = ticketTagsSet(provider, repoKey, id, splitTagCSV(*setCSV))
	case len(deltas) > 0:
		merged := applyTagDeltas(ticketTagsGet(provider, repoKey, id), deltas)
		stored = ticketTagsSet(provider, repoKey, id, merged)
	default:
		stored = ticketTagsGet(provider, repoKey, id)
	}

	for _, t := range stored {
		fmt.Println(t)
	}
	fmt.Fprintf(os.Stderr, "%d tag(s) on #%s\n", len(stored), id)
}

// splitTagCSV splits a comma-separated tag list, trimming blanks. Final
// normalization/validation is the server's job.
func splitTagCSV(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// applyTagDeltas folds +add / -remove tokens (and bare tokens, treated as adds)
// over the current set, returning the resulting set. Order/dedup don't matter —
// the server normalizes and dedups authoritatively.
func applyTagDeltas(cur, deltas []string) []string {
	set := make(map[string]bool, len(cur))
	for _, t := range cur {
		set[t] = true
	}
	for _, d := range deltas {
		switch {
		case strings.HasPrefix(d, "+"):
			if t := strings.TrimPrefix(d, "+"); t != "" {
				set[t] = true
			}
		case strings.HasPrefix(d, "-"):
			delete(set, strings.TrimPrefix(d, "-"))
		default:
			set[d] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ticketTagsGet fetches the stored tag set for a ticket over the daemon WS.
func ticketTagsGet(provider, repoKey, id string) []string {
	raw, err := daemonWSRequest("ticket_tags_get", map[string]interface{}{
		"provider":  provider,
		"repo_key":  repoKey,
		"opaque_id": id,
	}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	return decodeTagsReply(raw)
}

// ticketTagsSet replace-sets the tag set for a ticket over the daemon WS and
// returns the normalized, stored set the server echoes back.
func ticketTagsSet(provider, repoKey, id string, tags []string) []string {
	if tags == nil {
		tags = []string{}
	}
	raw, err := daemonWSRequest("ticket_tags_set", map[string]interface{}{
		"provider":  provider,
		"repo_key":  repoKey,
		"opaque_id": id,
		"tags":      tags,
	}, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	return decodeTagsReply(raw)
}

// decodeTagsReply parses a ticket_tags_*_response, exiting on a wire error.
func decodeTagsReply(raw json.RawMessage) []string {
	var resp struct {
		Tags  []string `json:"tags"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: invalid server response: %v\n", err)
		os.Exit(1)
	}
	if resp.Error != "" {
		ticketFail(resp.Error)
	}
	return resp.Tags
}
