//go:build darwin || linux

package main

import "testing"

func TestIsSlashCommand(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "system local_command",
			line: `{"type":"system","subtype":"local_command","message":{"content":"<command-name>/voice</command-name>"}}`,
			want: true,
		},
		{
			name: "user with command-name XML",
			line: `{"type":"user","message":{"content":"<command-name>/commit</command-name><command-message>commit</command-message><command-args></command-args>"}}`,
			want: true,
		},
		{
			name: "user with local-command-caveat XML",
			line: `{"type":"user","message":{"content":"<local-command-caveat>some caveat</local-command-caveat><command-name>/mcp</command-name>"}}`,
			want: true,
		},
		{
			name: "user with leading whitespace before command-name",
			line: `{"type":"user","message":{"content":"  <command-name>/voice</command-name>"}}`,
			want: true,
		},
		{
			name: "normal user message",
			line: `{"type":"user","message":{"content":"Please fix the bug in main.go"}}`,
			want: false,
		},
		{
			name: "assistant message",
			line: `{"type":"assistant","message":{"content":"I will fix the bug."}}`,
			want: false,
		},
		{
			name: "system non-local_command",
			line: `{"type":"system","subtype":"init","message":{"content":"session started"}}`,
			want: false,
		},
		{
			name: "invalid JSON passes through",
			line: `not json at all`,
			want: false,
		},
		{
			name: "empty line",
			line: ``,
			want: false,
		},
		{
			name: "user message with no message field",
			line: `{"type":"user"}`,
			want: false,
		},
		{
			name: "user message mentioning command-name mid-text",
			line: `{"type":"user","message":{"content":"The <command-name> tag is used for slash commands"}}`,
			want: false,
		},
		{
			name: "local_command with different type",
			line: `{"type":"assistant","subtype":"local_command"}`,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSlashCommand(tt.line)
			if got != tt.want {
				t.Errorf("isSlashCommand() = %v, want %v\nline: %s", got, tt.want, tt.line)
			}
		})
	}
}
