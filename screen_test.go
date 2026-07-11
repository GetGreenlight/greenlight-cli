//go:build darwin || linux

package main

import (
	"fmt"
	"strings"
	"testing"
)

// cup returns a cursor-position escape (1-based row;col).
func cup(row, col int) string { return fmt.Sprintf("\033[%d;%dH", row, col) }

const (
	clearScreen = "\033[2J"
	dim         = "\033[2m"
	italic      = "\033[3m"
	sgrReset    = "\033[0m"
)

const fg256grey = "\033[38;5;240m" // xterm-256 mid grey, the usual ghost colour
const fgANSIBrightBlack = "\033[90m"
const fgANSIBlack = "\033[30m"

// TestComposerSuggestion_Italic extracts an italic ghost suffix after the ❯
// prompt — the styling vt10x actually tracks.
func TestComposerSuggestion_Italic(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ " + italic + `Try "rename the variable"` + sgrReset))
	if got := s.composerSuggestion(); got != `Try "rename the variable"` {
		t.Fatalf("italic suggestion: got %q", got)
	}
}

// TestComposerSuggestion_GreyColor extracts a grey-foreground ghost suffix.
func TestComposerSuggestion_GreyColor(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ " + fg256grey + "Ask me anything" + sgrReset))
	if got := s.composerSuggestion(); got != "Ask me anything" {
		t.Fatalf("grey suggestion: got %q", got)
	}
}

// TestComposerSuggestion_TypedNotGhost is the discrimination that makes this
// worth doing: user-typed text (default styling) after the prompt must NOT be
// reported as a suggestion — only the styled ghost suffix is.
func TestComposerSuggestion_TypedNotGhost(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	// User typed "fix the " in default style; the ghost completion "login bug"
	// trails in grey.
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ fix the " + fg256grey + "login bug" + sgrReset))
	if got := s.composerSuggestion(); got != "login bug" {
		t.Fatalf("expected only the ghost suffix, got %q", got)
	}

	// No ghost at all — only typed text — yields no suggestion.
	s2 := newScreenTap("claude", 60, 8)
	s2.Write([]byte(clearScreen + cup(8, 1) + "❯ just typed text"))
	if got := s2.composerSuggestion(); got != "" {
		t.Fatalf("typed-only line should yield no suggestion, got %q", got)
	}
}

// TestComposerSuggestion_BlackTextNotGhost protects Codex's typed-input case:
// user text can render as black/dark terminal text, but prompt suggestions must
// only use the explicit ghost style.
func TestComposerSuggestion_BlackTextNotGhost(t *testing.T) {
	for name, sgr := range map[string]string{
		"black":        fgANSIBlack,
		"bright-black": fgANSIBrightBlack,
	} {
		t.Run(name, func(t *testing.T) {
			s := newScreenTap("codex", 60, 8)
			s.Write([]byte(clearScreen + cup(8, 1) + codexMarker + sgr + "typed prompt text" + sgrReset))
			if got := s.composerSuggestion(); got != "" {
				t.Fatalf("black typed text should not yield suggestion, got %q", got)
			}
		})
	}
}

// TestComposerSuggestion_CodexIgnoresBeforeCursor covers the live Codex repaint
// shape: typed text can inherit the placeholder's tracked style in vt10x even
// though the real terminal displays it as normal black text. For Codex, only
// ghost text at/after the active cursor is a prompt suggestion.
func TestComposerSuggestion_CodexIgnoresBeforeCursor(t *testing.T) {
	s := newScreenTap("codex", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + codexMarker + fg256grey + "hi there" + sgrReset + cup(8, 11)))
	if got := s.composerSuggestion(); got != "" {
		t.Fatalf("codex text before cursor should not yield suggestion, got %q", got)
	}
}

// TestAgentSupportsSuggestions locks the confirmed-agent gate (#38, widened to
// codex by #269): other agents must not have extraction wired until each is
// validated.
func TestAgentSupportsSuggestions(t *testing.T) {
	for _, a := range []string{"claude", "codex"} {
		if !agentSupportsSuggestions(a) {
			t.Errorf("%q should support suggestions", a)
		}
	}
	for _, a := range []string{"copilot", "cursor", "gemini", ""} {
		if agentSupportsSuggestions(a) {
			t.Errorf("%q should not support suggestions yet", a)
		}
	}
}

// codexMarker is codex's real composer-marker rendering, byte-for-byte as
// captured from a live `codex` PTY session (#269): bold + faint '›' (U+203A),
// reset back to default before the rest of the line.
const codexMarker = "\033[1m\033[2m›\033[39m\033[49m\033[0m"

// TestComposerText_Codex reads typed content after codex's real marker
// rendering — the fixture bytes are the exact escape sequence codex emits,
// not a synthetic guess (spec acceptance criterion, #269).
func TestComposerText_Codex(t *testing.T) {
	s := newScreenTap("codex", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + codexMarker + "hello world test"))
	got, seen := s.composerText()
	if !seen {
		t.Fatal("expected codex's marker to be seen")
	}
	if got != "hello world test" {
		t.Fatalf("typed text: got %q", got)
	}
}

// TestComposerSuggestion_CodexGhost extracts Codex's current faint placeholder
// rendering. Captured from codex-cli 0.142.5: the marker is bold, then the
// placeholder is painted with SGR 2.
func TestComposerSuggestion_CodexGhost(t *testing.T) {
	s := newScreenTap("codex", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + codexMarker + " " + dim + "Run /review on my current changes" + sgrReset + cup(8, 3)))
	if got := s.composerSuggestion(); got != "Run /review on my current changes" {
		t.Fatalf("codex ghost suggestion: got %q", got)
	}
}

// TestComposerSuggestion_CodexTypedNotGhost confirms typed Codex input is not
// reported as a suggested prompt. Codex paints typed input with default styling.
func TestComposerSuggestion_CodexTypedNotGhost(t *testing.T) {
	s := newScreenTap("codex", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + codexMarker + "hello world test"))
	if got := s.composerSuggestion(); got != "" {
		t.Fatalf("codex typed input should not report a ghost suggestion, got %q", got)
	}
}

// TestComposerSuggestion_CurrentComposerWins locks the regression where Codex
// left an older ghost row above the current typed composer row during repaint.
// The bottom-most composer marker is authoritative; if it has no ghost text,
// do not keep scanning upward and return a stale placeholder.
func TestComposerSuggestion_CurrentComposerWins(t *testing.T) {
	s := newScreenTap("codex", 80, 12)
	s.Write([]byte(clearScreen +
		cup(8, 1) + "\033[1m›\033[22m\033[2mRun /review on my current changes\033[0m" +
		cup(10, 1) + "\033[1m›\033[22mtyped text"))
	if got := s.composerSuggestion(); got != "" {
		t.Fatalf("typed current composer should suppress stale ghost row, got %q", got)
	}
}

// TestComposerMarker_AgentScoped: a claude tap must not recognise codex's
// marker glyph and vice versa — composerConfigs scopes matching per agent
// (#269), it isn't a single shared glyph set.
func TestComposerMarker_AgentScoped(t *testing.T) {
	claude := newScreenTap("claude", 60, 8)
	claude.Write([]byte(clearScreen + cup(8, 1) + codexMarker + "typed"))
	if _, seen := claude.composerText(); seen {
		t.Fatal("claude tap should not recognise codex's › marker")
	}

	codex := newScreenTap("codex", 60, 8)
	codex.Write([]byte(clearScreen + cup(8, 1) + "❯ typed"))
	if _, seen := codex.composerText(); seen {
		t.Fatal("codex tap should not recognise claude's ❯ marker")
	}
}

// TestAgentIdleMarkerVisible locks the #269 autopilot compatibility flag:
// claude supports a pre-injection marker wait, while codex skips that wait so
// versions that don't paint an idle marker still enter the verified loop.
func TestAgentIdleMarkerVisible(t *testing.T) {
	if !agentIdleMarkerVisible("claude") {
		t.Error("claude should paint its marker while idle")
	}
	if agentIdleMarkerVisible("codex") {
		t.Error("codex should skip the pre-injection marker wait")
	}
}

// TestComposerSuggestion_Faint is the real Claude Code case (#38): the ghost
// text is rendered with SGR 2 (faint) over the default colour. vt10x has no
// faint handler and would drop it, leaving the cells indistinguishable from
// typed text — so rewriteFaintToGrey (in Write) substitutes a tracked grey
// foreground, making the suggestion detectable. This is the exact styling the
// daemon-log composer dump revealed.
func TestComposerSuggestion_Faint(t *testing.T) {
	s := newScreenTap("claude", 80, 20)
	ghost := "send a throwaway message then retype its opening"
	s.Write([]byte(clearScreen + cup(16, 1) + "❯ " + dim + ghost + sgrReset))
	if got := s.composerSuggestion(); got != ghost {
		t.Fatalf("faint ghost text not detected, got %q", got)
	}
}

// TestComposerSuggestion_FaintSplitWrites checks a faint SGR sequence split
// across two Write calls (a PTY read boundary landing mid-escape) is still
// rewritten via the carry buffer.
func TestComposerSuggestion_FaintSplitWrites(t *testing.T) {
	s := newScreenTap("claude", 80, 20)
	ghost := "retype its opening"
	full := clearScreen + cup(16, 1) + "❯ " + dim + ghost + sgrReset
	cut := strings.Index(full, dim) + 1 // split between ESC and '[' of the faint SGR
	s.Write([]byte(full[:cut]))
	s.Write([]byte(full[cut:]))
	if got := s.composerSuggestion(); got != ghost {
		t.Fatalf("faint ghost split across writes not detected, got %q", got)
	}
}

// TestRewriteFaintToGrey covers the SGR rewrite in isolation: only a standalone
// faint param is swapped, and non-SGR / non-faint sequences pass through.
func TestRewriteFaintToGrey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\033[2m", "\033[38;5;240m"},         // bare faint
		{"\033[0;2m", "\033[0;38;5;240m"},     // reset + faint
		{"\033[1;2;4m", "\033[1;38;5;240;4m"}, // faint among others
		{"\033[22m", "\033[22m"},              // normal-intensity, not faint
		{"\033[2J", "\033[2J"},                // erase display — not SGR
		{"\033[38;5;105m", "\033[38;5;105m"},  // explicit colour untouched
		{"plain text", "plain text"},          // no escape
	}
	for _, c := range cases {
		out, carry := rewriteFaintToGrey(nil, []byte(c.in))
		if len(carry) != 0 {
			t.Errorf("%q: unexpected carry %q", c.in, carry)
		}
		if string(out) != c.want {
			t.Errorf("rewrite %q = %q, want %q", c.in, out, c.want)
		}
	}
}

// TestComposerText_Typed reads the user-typed (default-styled) run after the ❯
// marker — the read the #221 closed-loop injector uses to confirm its prompt
// landed. markerSeen must be true since a composer is present.
func TestComposerText_Typed(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ implement the parser fix"))
	got, seen := s.composerText()
	if !seen {
		t.Fatal("expected a composer marker to be seen")
	}
	if got != "implement the parser fix" {
		t.Fatalf("typed text: got %q", got)
	}
}

// TestComposerText_GhostOnly is the discrimination that makes this useful: a
// composer showing only a ghost suggestion (faint) holds no user text, so
// composerText must return "" (with markerSeen true) — i.e. "still empty".
func TestComposerText_GhostOnly(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ " + dim + "Try \"add a test\"" + sgrReset))
	got, seen := s.composerText()
	if !seen {
		t.Fatal("expected a composer marker to be seen")
	}
	if got != "" {
		t.Fatalf("ghost-only composer should yield no typed text, got %q", got)
	}
}

// TestComposerText_Empty: a bare marker with nothing after it is an empty
// composer — marker seen, no text.
func TestComposerText_Empty(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ "))
	got, seen := s.composerText()
	if !seen {
		t.Fatal("expected a composer marker to be seen")
	}
	if got != "" {
		t.Fatalf("empty composer should yield no typed text, got %q", got)
	}
}

// TestComposerText_MixedTypedAndGhost: when the line carries typed text followed
// by a ghost completion, only the typed run is returned (the ghost is dropped).
func TestComposerText_MixedTypedAndGhost(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(8, 1) + "❯ fix the " + fg256grey + "login bug" + sgrReset))
	got, seen := s.composerText()
	if !seen {
		t.Fatal("expected a composer marker to be seen")
	}
	if got != "fix the" {
		t.Fatalf("expected only the typed run, got %q", got)
	}
}

// TestComposerText_NoMarker: a screen with no composer marker at all (the
// mock_claude / non-claude case) reports markerSeen==false, which the injector
// uses to fall back to open-loop delivery rather than spin in a retry loop.
func TestComposerText_NoMarker(t *testing.T) {
	s := newScreenTap("claude", 60, 8)
	s.Write([]byte(clearScreen + cup(1, 1) + "just some transcript output, no composer"))
	got, seen := s.composerText()
	if seen {
		t.Fatalf("expected no composer marker, got text %q", got)
	}
	if got != "" {
		t.Fatalf("no-marker read should yield empty text, got %q", got)
	}
}
