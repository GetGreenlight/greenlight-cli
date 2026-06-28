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
	if got := greenlightSystemPrompt(nil, nil); got != greenlightSystemPromptBase {
		t.Errorf("no-shim prompt diverged from base:\n got: %q", got)
	}
	if got := greenlightSystemPrompt(nil, []resolvedShim{}); got != greenlightSystemPromptBase {
		t.Errorf("empty-shim prompt diverged from base:\n got: %q", got)
	}
}

func TestGreenlightSystemPromptShimLine(t *testing.T) {
	shims := []resolvedShim{
		{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"},
		{cmd: "glab", secret: "GITLAB_ACCESS_TOKEN", envName: "GITLAB_TOKEN"},
	}
	got := greenlightSystemPrompt(nil, shims)
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
	got := greenlightSystemPrompt(ticket, shims)
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
