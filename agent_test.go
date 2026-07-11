//go:build darwin || linux

package main

import (
	"strings"
	"testing"
)

func TestAgentSupportsResume(t *testing.T) {
	tests := []struct {
		agent    string
		expected bool
	}{
		{"claude", true},
		{"copilot", true},
		{"codex", true},
		{"cursor", true},
		{"gemini", true},
		{"pi", false},
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			got := agentSupportsResume(tt.agent)
			if got != tt.expected {
				t.Errorf("agentSupportsResume(%q) = %v, want %v", tt.agent, got, tt.expected)
			}
		})
	}
}

func TestGreenlightSystemPromptNoShimUnchanged(t *testing.T) {
	// With no active shim the prompt must be byte-for-byte the base prompt —
	// the agent is never told a CLI is pre-authenticated when it would fall
	// through to its own ambient auth (#198).
	if got := greenlightSystemPrompt(nil, nil, sshSession{}); got != greenlightSystemPromptBase {
		t.Errorf("no-shim prompt diverged from base:\n got: %q", got)
	}
	if got := greenlightSystemPrompt(nil, []resolvedShim{}, sshSession{}); got != greenlightSystemPromptBase {
		t.Errorf("empty-shim prompt diverged from base:\n got: %q", got)
	}
}

func TestGreenlightSystemPromptShimLine(t *testing.T) {
	shims := []resolvedShim{
		{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"},
		{cmd: "glab", secret: "GITLAB_ACCESS_TOKEN", envName: "GITLAB_TOKEN"},
	}
	got := greenlightSystemPrompt(nil, shims, sshSession{})
	if !strings.HasPrefix(got, greenlightSystemPromptBase) {
		t.Fatalf("prompt must extend the base prompt; got:\n%q", got)
	}
	if !strings.Contains(got, "pre-authenticated") {
		t.Errorf("prompt missing pre-authenticated line:\n%q", got)
	}
	// Lists exactly the active shim commands, no others.
	if !strings.Contains(got, "gh, glab") {
		t.Errorf("prompt should list the active shim commands 'gh, glab':\n%q", got)
	}
	// The shim line itself must not leak the secret/env names — the agent runs
	// the command bare; greenlight injects the token. (The base prompt mentions
	// GITHUB_ACCESS_TOKEN as a generic example, so only inspect the suffix.)
	suffix := strings.TrimPrefix(got, greenlightSystemPromptBase)
	if strings.Contains(suffix, "GITHUB_ACCESS_TOKEN") || strings.Contains(suffix, "GH_TOKEN") {
		t.Errorf("shim line leaked a secret/env name:\n%q", suffix)
	}
}

func TestGreenlightSystemPromptShimAndTicket(t *testing.T) {
	shims := []resolvedShim{{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"}}
	ticket := &TicketRef{URL: "https://example.test/issues/1"}
	got := greenlightSystemPrompt(ticket, shims, sshSession{})
	if !strings.Contains(got, "pre-authenticated") {
		t.Errorf("prompt missing shim line:\n%q", got)
	}
	if !strings.Contains(got, ticket.URL) {
		t.Errorf("prompt missing ticket URL:\n%q", got)
	}
	// Shim line precedes the ticket line.
	if strings.Index(got, "pre-authenticated") > strings.Index(got, ticket.URL) {
		t.Errorf("shim line should precede ticket line:\n%q", got)
	}
}

func TestShimPreauthLineEmpty(t *testing.T) {
	if got := shimPreauthLine(nil); got != "" {
		t.Errorf("shimPreauthLine(nil) = %q, want empty", got)
	}
}

func TestGreenlightSystemPromptSSHIsolation(t *testing.T) {
	// Off is the opt-in promise: byte-for-byte the base prompt (#249).
	if got := greenlightSystemPrompt(nil, nil, sshSession{}); got != greenlightSystemPromptBase {
		t.Errorf("isolation-off prompt diverged from base:\n got: %q", got)
	}
	// On with no keys appends exactly the one no-identity line.
	got := greenlightSystemPrompt(nil, nil, sshSession{isolated: true})
	want := greenlightSystemPromptBase + "\n\n" + sshNoIdentityLine
	if got != want {
		t.Errorf("isolation-on prompt = %q, want base + no-identity line", got)
	}
}

func TestGreenlightSystemPromptSSHManagedKeys(t *testing.T) {
	// On with served keys appends the managed-agent line (short names), not
	// the no-identity line (#250).
	st := sshSession{isolated: true, keys: []sshSessionKey{
		{name: "staging", secretName: "SSH_KEY_STAGING"},
		{name: "ci", secretName: "SSH_KEY_CI"},
	}}
	got := greenlightSystemPrompt(nil, nil, st)
	want := greenlightSystemPromptBase + "\n\n" + sshManagedAgentLine([]string{"staging", "ci"})
	if got != want {
		t.Errorf("managed-keys prompt = %q, want base + managed line", got)
	}
	if strings.Contains(got, sshNoIdentityLine) {
		t.Error("managed-keys prompt must not carry the no-identity line")
	}
	if !strings.Contains(got, "(keys: staging, ci)") {
		t.Errorf("managed line should name the short keys: %q", got)
	}
}

func TestGreenlightSystemPromptSSHSkippedKeys(t *testing.T) {
	// On with ssh_keys configured but none resolved appends the skipped-keys
	// line (naming the unresolvable secrets), not the generic no-identity
	// line — a user who configured a key should be told it failed to load,
	// not left indistinguishable from never having configured one (#292).
	st := sshSession{isolated: true, skipped: []string{"SSH_KEY_STAGING"}}
	got := greenlightSystemPrompt(nil, nil, st)
	want := greenlightSystemPromptBase + "\n\n" + sshSkippedKeysLine([]string{"SSH_KEY_STAGING"})
	if got != want {
		t.Errorf("skipped-keys prompt = %q, want base + skipped line", got)
	}
	if strings.Contains(got, sshNoIdentityLine) {
		t.Error("skipped-keys prompt must not carry the generic no-identity line")
	}
	if !strings.Contains(got, "SSH_KEY_STAGING") {
		t.Errorf("skipped-keys line should name the unresolved secret: %q", got)
	}
}

func TestGreenlightSystemPromptSSHIsolationOrdering(t *testing.T) {
	// With shims, isolation, and a ticket all active, all three lines are
	// present after the base: shim line, then ssh line, then ticket line.
	shims := []resolvedShim{{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"}}
	ticket := &TicketRef{URL: "https://example.test/issues/1"}
	got := greenlightSystemPrompt(ticket, shims, sshSession{isolated: true})
	if !strings.HasPrefix(got, greenlightSystemPromptBase) {
		t.Fatalf("prompt must extend the base prompt; got:\n%q", got)
	}
	shimIdx := strings.Index(got, "pre-authenticated")
	sshIdx := strings.Index(got, sshNoIdentityLine)
	ticketIdx := strings.Index(got, ticket.URL)
	if shimIdx < 0 || sshIdx < 0 || ticketIdx < 0 {
		t.Fatalf("prompt missing a line (shim=%d ssh=%d ticket=%d):\n%q", shimIdx, sshIdx, ticketIdx, got)
	}
	if !(shimIdx < sshIdx && sshIdx < ticketIdx) {
		t.Errorf("line order should be shim < ssh < ticket (shim=%d ssh=%d ticket=%d):\n%q", shimIdx, sshIdx, ticketIdx, got)
	}
}
