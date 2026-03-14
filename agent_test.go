//go:build darwin || linux

package main

import "testing"

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
