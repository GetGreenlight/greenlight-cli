package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// buildConnectCommand must hand off the autopilot name, ticket, and prompt-file
// (#142, #4) so the spawned connect names the session by role and injects the
// stage prompt. The vars travel as an inline `VAR=val cmd` env prefix on the
// connect command only — never `export`ed (#195), which would leak them into the
// spawned shell and misname a later manual session. The prompt prose itself
// travels via a temp file, never inline.
func TestBuildConnectCommand_AutopilotExports(t *testing.T) {
	ticket := &TicketRef{Provider: "github", OpaqueID: "142", URL: "https://github.com/o/r/issues/142"}
	cmd := buildConnectCommand("/usr/local/bin/greenlight", "/work/repo", "claude", "dev-1",
		ticket, "/tmp/greenlight-initprompt-abc.txt", "Implementer for #142")

	wants := []string{
		"GREENLIGHT_TICKET_JSON=",
		"GREENLIGHT_SESSION_NAME=",
		"Implementer for #142",
		"GREENLIGHT_INITIAL_PROMPT_FILE=",
		"/tmp/greenlight-initprompt-abc.txt",
		"--device-id ",
		"--agent ",
		"connect ",
	}
	for _, w := range wants {
		if !strings.Contains(cmd, w) {
			t.Errorf("connect cmd missing %q\ngot: %s", w, cmd)
		}
	}
	// The handoff vars must NOT be exported (#195) — an export leaks them into the
	// spawned terminal's persistent shell, so a later manual connect inherits a
	// stale name/ticket. They must ride an inline prefix instead.
	for _, unwanted := range []string{
		"export GREENLIGHT_TICKET_JSON",
		"export GREENLIGHT_SESSION_NAME",
		"export GREENLIGHT_INITIAL_PROMPT_FILE",
	} {
		if strings.Contains(cmd, unwanted) {
			t.Errorf("connect cmd exports a handoff var (#195 leak); expected inline prefix\nfound: %q\ngot: %s", unwanted, cmd)
		}
	}
	// The inline env prefix must sit after `cd <cwd> &&`, immediately before the
	// quoted <exe> word — so the assignments apply to the connect process only.
	cdIdx := strings.Index(cmd, "&& ")
	exeIdx := strings.Index(cmd, "/usr/local/bin/greenlight' connect")
	nameIdx := strings.Index(cmd, "GREENLIGHT_SESSION_NAME=")
	if cdIdx < 0 || exeIdx < 0 || nameIdx < 0 || !(cdIdx < nameIdx && nameIdx < exeIdx) {
		t.Errorf("inline env prefix not positioned between `cd &&` and the connect exe\ngot: %s", cmd)
	}
	// The prompt prose must never be embedded inline — that is the whole point
	// of the file handoff (#4). Only the fixed-shape file path may appear.
	if strings.Contains(cmd, "GREENLIGHT_INITIAL_PROMPT=") {
		t.Errorf("connect cmd carries the inline prompt var; expected file handoff only\ngot: %s", cmd)
	}
}

// escapeAppleScriptString must escape backslashes (not just double quotes) so a
// connect command carrying shellQuote'd apostrophes (rendered as '\'') stays a
// parseable AppleScript string literal. A missing backslash escape made
// osascript fail with syntax error -2741, so autopilot sessions never spawned.
func TestEscapeAppleScriptString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`plain`, `plain`},
		{`say "hi"`, `say \"hi\"`},
		// A real autopilot prompt fragment: shellQuote turns it's -> it'\''s.
		{`export X='it'\''s good'`, `export X='it'\\''s good'`},
		{`a\b"c`, `a\\b\"c`},
	}
	for _, c := range cases {
		if got := escapeAppleScriptString(c.in); got != c.want {
			t.Errorf("escapeAppleScriptString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Without autopilot fields, none of the autopilot env exports appear.
func TestBuildConnectCommand_NoAutopilotExports(t *testing.T) {
	cmd := buildConnectCommand("/usr/local/bin/greenlight", "/work/repo", "", "", nil, "", "")
	for _, unwanted := range []string{"GREENLIGHT_SESSION_NAME", "GREENLIGHT_INITIAL_PROMPT", "GREENLIGHT_INITIAL_PROMPT_FILE", "GREENLIGHT_TICKET_JSON", "--agent ", "--device-id "} {
		if strings.Contains(cmd, unwanted) {
			t.Errorf("connect cmd unexpectedly contains %q\ngot: %s", unwanted, cmd)
		}
	}
	if !strings.Contains(cmd, "connect") {
		t.Errorf("connect cmd missing the connect verb: %s", cmd)
	}
}

// spawnQuotingFixtures cover every special-char class the real autopilot stage
// prompts and session names use. They are stressed through the full spawn
// pipeline (shellQuote inside buildConnectCommand → escapeAppleScriptString →
// osascript) to lock the quoting invariant: the shell command the terminal sees
// must be byte-identical to the one the daemon intended, regardless of content.
var spawnQuotingFixtures = []struct {
	name  string
	value string
}{
	{"apostrophe", "don't reject it's the reviewer's call"},
	{"backtick", "run `go test ./...` then `git commit`"},
	{"em-dash", "spec looks good — proceed to code—now"},
	{"parens", "implement foo() and bar(x, y) (carefully)"},
	{"angle-brackets", "compare <expected> vs <actual> where a<b && c>d"},
	{"double-quote", `wrap the "title" field in quotes`},
	{"backslash", `path is C:\work\repo and regex \d+\.\d+`},
	{"mixed", "Reviewer's note: `diff` shows <foo> — fix (a,b) & don't \"panic\""},
	{"long-600", strings.Repeat("Don't ship it — verify `tests` pass (all of them) <first>. ", 12)},
}

// TestSpawnCommandQuotingByteIdentical is the #4 regression test. Each fixture is
// carried inline through buildConnectCommand (as the session name — the one
// free-form field that still travels in the typed command after the prompt moved
// to a file handoff) and the full command is escaped for AppleScript. The shell
// command the terminal would run must come back byte-identical:
//   - always: escapeAppleScriptString must not touch single quotes (only \ and "),
//     so the shellQuote'd '\'' apostrophe runs survive intact.
//   - macOS: the escaped literal is round-tripped through a real osascript
//     `return "…"` and must equal the intended command exactly. This is the leg
//     that fails against the pre-9bff115 quotes-only escaping (osascript syntax
//     error -2741 → empty/garbled output, no spawn).
func TestSpawnCommandQuotingByteIdentical(t *testing.T) {
	for _, f := range spawnQuotingFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			intended := buildConnectCommand("/usr/local/bin/greenlight", "/work/repo",
				"claude", "dev-1", nil, "", f.value)

			// Sanity: the fixture must actually reach the command (e.g. the long
			// one isn't truncated).
			if !strings.Contains(intended, "GREENLIGHT_SESSION_NAME=") {
				t.Fatalf("fixture %q did not produce a session-name export\ngot: %s", f.name, intended)
			}

			escaped := escapeAppleScriptString(intended)

			// AppleScript escaping only doubles \ and escapes "; it must leave the
			// shell's single quotes (including the '\'' apostrophe runs) untouched,
			// else shellQuote'd content would be corrupted before it ever reaches
			// the shell.
			if got, want := strings.Count(escaped, "'"), strings.Count(intended, "'"); got != want {
				t.Errorf("escaping changed single-quote count: intended=%d escaped=%d\nintended: %s", want, got, intended)
			}

			if runtime.GOOS == "darwin" {
				roundTripped := osascriptReturn(t, escaped)
				if roundTripped != intended {
					t.Errorf("osascript round-trip mismatch for %q\nintended:  %q\nround-trip: %q", f.name, intended, roundTripped)
				}
			}
		})
	}
}

// osascriptReturn embeds an AppleScript-escaped string literal in `return "…"`,
// runs it through osascript, and returns the string osascript parsed back out.
// If escaping is correct, the result equals the original (pre-escape) string.
func osascriptReturn(t *testing.T, escaped string) string {
	t.Helper()
	out, err := exec.Command("osascript", "-e", `return "`+escaped+`"`).Output()
	if err != nil {
		// A parse failure (the actual #4 symptom under the old escaping) surfaces
		// here as a non-zero exit — fail loudly rather than silently.
		t.Fatalf("osascript failed (escaping likely broken): %v\nescaped literal: %s", err, escaped)
	}
	// osascript prints the string value followed by a single trailing newline.
	return strings.TrimSuffix(string(out), "\n")
}

// greenlightPromptSample mirrors the #5 greenlight code-reviewer stage prompt's
// special-char class — backticks (`greenlight ticket merge --branch …`) and an
// apostrophe ("reviewer's") — the exact characters that broke the #4 inline
// spawn. It must survive the temp-file handoff (the only path prompt prose
// travels) byte-for-byte.
const greenlightPromptSample = "Locate the implementer's branch by the `ticket-<id>-*` naming convention and merge it with `greenlight ticket merge --branch ticket-<id>-<slug>` — append the reviewer's notes to the ticket body if it needs rework."

// TestWriteInitialPromptFile round-trips free-form prompts through the temp-file
// handoff: the file lands under $TMPDIR, holds the prompt verbatim, and a reader
// (as daemon newSession does) can recover it and unlink it. Covers the long,
// special-char-heavy fixture and the #5 greenlight prompt sample so the
// backtick/apostrophe prose stays exact off the prose-by-file path.
func TestWriteInitialPromptFile(t *testing.T) {
	prompts := []string{
		spawnQuotingFixtures[len(spawnQuotingFixtures)-1].value, // the long, special-char-heavy one
		greenlightPromptSample,
	}
	for _, prompt := range prompts {
		path, err := writeInitialPromptFile(prompt)
		if err != nil {
			t.Fatalf("writeInitialPromptFile: %v", err)
		}
		if !strings.HasPrefix(path, os.TempDir()) {
			t.Errorf("prompt file %q not under TMPDIR %q", path, os.TempDir())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back prompt file: %v", err)
		}
		if string(data) != prompt {
			t.Errorf("prompt file content mismatch\nwant: %q\ngot:  %q", prompt, string(data))
		}
		os.Remove(path)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("prompt file still present after removal: %v", err)
		}
	}
}
