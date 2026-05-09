//go:build darwin || linux

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// ---------- transcript transformers ----------

// claudeFrame is the subset of Claude's wire format the tests assert on.
// All transformers must produce something that unmarshals to this shape
// with `type` set to user/assistant.
type claudeFrame struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// assertClaudeFrame parses the transformer output and verifies it has
// the expected role + that content (string or array) contains needle.
func assertClaudeFrame(t *testing.T, out, wantType, needle string) {
	t.Helper()
	if out == "" {
		t.Fatal("transformer returned empty string")
	}
	var f claudeFrame
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatalf("output is not valid JSON: %v\nout=%s", err, out)
	}
	if f.Type != wantType {
		t.Errorf("type = %q, want %q (out=%s)", f.Type, wantType, out)
	}
	if f.Message.Role != wantType {
		t.Errorf("message.role = %q, want %q", f.Message.Role, wantType)
	}
	if !strings.Contains(string(f.Message.Content), needle) {
		t.Errorf("content does not contain %q: %s", needle, string(f.Message.Content))
	}
}

func TestTransformCopilotEvent(t *testing.T) {
	t.Run("user message", func(t *testing.T) {
		line := `{"type":"user.message","id":"u1","timestamp":"2026-05-09T00:00:00Z","data":{"content":"hello copilot"}}`
		assertClaudeFrame(t, transformCopilotEvent(line), "user", "hello copilot")
	})
	t.Run("assistant message", func(t *testing.T) {
		line := `{"type":"assistant.message","id":"a1","timestamp":"2026-05-09T00:00:00Z","data":{"content":"hi human","model":"gpt-4"}}`
		assertClaudeFrame(t, transformCopilotEvent(line), "assistant", "hi human")
	})
	t.Run("garbage", func(t *testing.T) {
		if got := transformCopilotEvent("not json"); got != "" {
			t.Errorf("garbage input should yield \"\", got %q", got)
		}
	})
	t.Run("unknown event type is dropped", func(t *testing.T) {
		if got := transformCopilotEvent(`{"type":"unknown.thing","data":{}}`); got != "" {
			t.Errorf("unknown event should yield \"\", got %q", got)
		}
	})
}

func TestTransformCursorEvent(t *testing.T) {
	t.Run("user message strips XML wrapper", func(t *testing.T) {
		line := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>fix the bug</user_query>"}]}}`
		out := transformCursorEvent(line)
		assertClaudeFrame(t, out, "user", "fix the bug")
		if strings.Contains(out, "<user_query>") {
			t.Errorf("expected XML wrapper stripped, got %s", out)
		}
	})
	t.Run("assistant message", func(t *testing.T) {
		line := `{"role":"assistant","message":{"content":[{"type":"text","text":"sure"}],"model":"cursor-large"}}`
		assertClaudeFrame(t, transformCursorEvent(line), "assistant", "sure")
	})
	t.Run("garbage", func(t *testing.T) {
		if got := transformCursorEvent("nope"); got != "" {
			t.Errorf("garbage should yield \"\", got %q", got)
		}
	})
	t.Run("unknown role is dropped", func(t *testing.T) {
		if got := transformCursorEvent(`{"role":"system","message":{"content":[]}}`); got != "" {
			t.Errorf("unknown role should yield \"\", got %q", got)
		}
	})
}

func TestTransformCodexEvent(t *testing.T) {
	t.Run("user message", func(t *testing.T) {
		line := `{"timestamp":"2026-05-09T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello codex"}]}}`
		assertClaudeFrame(t, transformCodexEvent(line), "user", "hello codex")
	})
	t.Run("user message with greenlight system context is filtered", func(t *testing.T) {
		// Codex injects environment_context wrappers — those should be
		// dropped, leaving any real user text behind.
		line := `{"timestamp":"2026-05-09T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd=/foo</environment_context>"},{"type":"input_text","text":"real prompt"}]}}`
		assertClaudeFrame(t, transformCodexEvent(line), "user", "real prompt")
	})
	t.Run("garbage", func(t *testing.T) {
		if got := transformCodexEvent("definitely not json"); got != "" {
			t.Errorf("garbage should yield \"\", got %q", got)
		}
	})
}

func TestTransformCodexEventReplay_StripsLiveOnlyEvents(t *testing.T) {
	// In replay mode, the live event_msg user_message/agent_message
	// frames are dropped (they're duplicated by response_item).
	t.Run("strips event_msg user_message", func(t *testing.T) {
		line := `{"type":"event_msg","payload":{"type":"user_message"}}`
		if got := transformCodexEventReplay(line); got != "" {
			t.Errorf("event_msg user_message should be dropped, got %q", got)
		}
	})
	t.Run("strips event_msg agent_message", func(t *testing.T) {
		line := `{"type":"event_msg","payload":{"type":"agent_message"}}`
		if got := transformCodexEventReplay(line); got != "" {
			t.Errorf("event_msg agent_message should be dropped, got %q", got)
		}
	})
	t.Run("passes through response_item", func(t *testing.T) {
		line := `{"timestamp":"2026-05-09T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
		assertClaudeFrame(t, transformCodexEventReplay(line), "user", "hi")
	})
}

func TestTransformPiEvent(t *testing.T) {
	t.Run("user message string content", func(t *testing.T) {
		line := `{"type":"message","id":"m1","timestamp":"2026-05-09T00:00:00Z","message":{"role":"user","content":"hello pi"}}`
		assertClaudeFrame(t, transformPiEvent(line), "user", "hello pi")
	})
	t.Run("user message array content", func(t *testing.T) {
		line := `{"type":"message","id":"m2","timestamp":"2026-05-09T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]}}`
		out := transformPiEvent(line)
		assertClaudeFrame(t, out, "user", "part one")
		if !strings.Contains(out, "part two") {
			t.Errorf("expected both parts joined, got %s", out)
		}
	})
	t.Run("non-message event dropped", func(t *testing.T) {
		if got := transformPiEvent(`{"type":"session_start","sessionId":"abc"}`); got != "" {
			t.Errorf("non-message event should yield \"\", got %q", got)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if got := transformPiEvent("garbage"); got != "" {
			t.Errorf("garbage should yield \"\", got %q", got)
		}
	})
}

func TestTransformGeminiMessage(t *testing.T) {
	t.Run("user message", func(t *testing.T) {
		// Gemini messages have a `type` field ("user" / "gemini") and a
		// nested content array. Not OpenAI/Anthropic role conventions.
		raw := json.RawMessage(`{"id":"u1","timestamp":"2026-05-09T00:00:00Z","type":"user","content":[{"text":"hello gemini"}]}`)
		entries := transformGeminiMessage(raw, "session-1")
		if len(entries) == 0 {
			t.Fatal("expected at least one entry")
		}
		got, _ := json.Marshal(entries[0])
		assertClaudeFrame(t, string(got), "user", "hello gemini")
	})
	t.Run("gemini-role becomes assistant", func(t *testing.T) {
		// gemini-side content is a JSON-encoded string, not an array.
		raw := json.RawMessage(`{"id":"a1","timestamp":"2026-05-09T00:00:00Z","type":"gemini","content":"hi human","model":"gemini-2.5"}`)
		entries := transformGeminiMessage(raw, "session-1")
		if len(entries) == 0 {
			t.Fatal("expected at least one entry")
		}
		got, _ := json.Marshal(entries[0])
		assertClaudeFrame(t, string(got), "assistant", "hi human")
	})
	t.Run("garbage returns empty", func(t *testing.T) {
		entries := transformGeminiMessage(json.RawMessage("not json"), "session-1")
		if len(entries) != 0 {
			t.Errorf("expected 0 entries for garbage, got %d", len(entries))
		}
	})
}
