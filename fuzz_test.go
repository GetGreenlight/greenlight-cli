//go:build darwin || linux

package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// addSeedCorpus feeds every file under testdata/corpus/<name>/ into the fuzzer
// as a seed. These files hold real agent output harvested by
// tests/gather_fuzz_corpus.py — committing them gives the fuzzer valid
// structure to mutate from. The directory is optional: a checkout without a
// harvested corpus simply runs with the hand-written f.Add seeds.
func addSeedCorpus(f *testing.F, name string, asBytes bool) {
	f.Helper()
	dir := filepath.Join("testdata", "corpus", name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if asBytes {
			f.Add(data)
		} else {
			f.Add(string(data))
		}
	}
}

// Fuzz targets for the parsers that consume adversarial / agent-controlled
// input. Run as normal tests in CI (they replay their seed corpus); run as
// real fuzzers on demand with:
//
//	go test -run xxx -fuzz FuzzUnwrapEvalCommand -fuzztime 30s
//
// Crashers found by the fuzzer land in testdata/fuzz/ and become permanent
// regression cases.

// --- Command unwrappers (interpose.go) -------------------------------------
//
// These do string surgery on each agent's shell wrapper to recover the real
// command before it is gated. The security-relevant invariant is conservative
// extraction: the unwrapper must never panic and must never return the empty
// string (an empty command would be gated as a no-op while the shell still
// runs the real thing). On any input it does not understand it must return
// the original cmd unchanged.

func checkUnwrap(t *testing.T, name, in, out string) {
	t.Helper()
	if out == "" && in != "" {
		t.Fatalf("%s returned empty for non-empty input %q", name, in)
	}
}

func FuzzUnwrapEvalCommand(f *testing.F) {
	f.Add(`cd /x && eval 'echo hi' \< /dev/null && pwd`)
	f.Add(`cd /x && eval "echo hi" \< /dev/null && pwd`)
	f.Add(`cd /x && eval 'echo '"'"'q'"'"'' \< /dev/null`)
	f.Add(`cd /x && eval 'rm -rf /' \< /dev/null`)
	f.Add(`plain command, no wrapper`)
	f.Add(`&& eval '`)
	f.Add(`&& eval "`)
	f.Add(``)
	addSeedCorpus(f, "wrappers", false)
	f.Fuzz(func(t *testing.T, cmd string) {
		out := unwrapEvalCommand(cmd)
		checkUnwrap(t, "unwrapEvalCommand", cmd, out)
	})
}

func FuzzUnwrapGeminiCommand(f *testing.F) {
	f.Add(`shopt -u foo; { echo hi }; __code=$?; exit $__code`)
	f.Add(`shopt -u foo; { echo } weird }; __code=$?`)
	f.Add(`shopt without a brace`)
	f.Add(`shopt -u; { `)
	f.Add(``)
	addSeedCorpus(f, "wrappers", false)
	f.Fuzz(func(t *testing.T, cmd string) {
		out := unwrapGeminiCommand(cmd)
		checkUnwrap(t, "unwrapGeminiCommand", cmd, out)
	})
}

func FuzzUnwrapCodexCommand(f *testing.F) {
	f.Add("if . '/x/snap.sh'; then :; fi\nexec '/bin/zsh' -c 'echo hi'")
	f.Add("\nexec '/bin/zsh' -c 'echo '\\''q'\\'''")
	f.Add("\nexec '")
	f.Add("\nexec '/bin/zsh' -c '")
	f.Add(`no wrapper here`)
	f.Add(``)
	addSeedCorpus(f, "wrappers", false)
	f.Fuzz(func(t *testing.T, cmd string) {
		out := unwrapCodexCommand(cmd)
		checkUnwrap(t, "unwrapCodexCommand", cmd, out)
	})
}

// --- Transcript transformers (stream.go) -----------------------------------
//
// These consume transcript files written by each agent. Format drifts between
// agent versions, so a malformed or unexpected entry must not panic the
// streamer. Invariant: never panic, and the output is either "" (skip) or a
// single valid JSON object in Claude's wire format.

func checkTransform(t *testing.T, name, out string) {
	t.Helper()
	if out == "" {
		return
	}
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("%s produced non-JSON output %q: %v", name, out, err)
	}
}

func FuzzTransformCopilotEvent(f *testing.F) {
	f.Add(`{"type":"user.message","id":"1","data":{"content":"hi"}}`)
	f.Add(`{"type":"assistant.message","data":{"content":"x","model":"m"}}`)
	f.Add(`{"type":"tool.execution_start","data":{"toolName":"shell","arguments":{}}}`)
	f.Add(`{"type":"unknown"}`)
	f.Add(`not json`)
	f.Add(``)
	addSeedCorpus(f, "copilot", false)
	f.Fuzz(func(t *testing.T, line string) {
		checkTransform(t, "transformCopilotEvent", transformCopilotEvent(line))
	})
}

func FuzzTransformCursorEvent(f *testing.F) {
	f.Add(`{"type":"assistant","message":{"content":"hi"}}`)
	f.Add(`{"type":"tool_call"}`)
	f.Add(`<thinking>x</thinking>`)
	f.Add(`not json`)
	f.Add(``)
	addSeedCorpus(f, "cursor", false)
	f.Fuzz(func(t *testing.T, line string) {
		checkTransform(t, "transformCursorEvent", transformCursorEvent(line))
	})
}

func FuzzTransformCodexEvent(f *testing.F) {
	f.Add(`{"type":"message","role":"user"}`)
	f.Add(`{"type":"function_call","name":"shell"}`)
	f.Add(`not json`)
	f.Add(``)
	addSeedCorpus(f, "codex", false)
	f.Fuzz(func(t *testing.T, line string) {
		// transformCodexEvent may emit multiple newline-separated objects.
		out := transformCodexEvent(line)
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			checkTransform(t, "transformCodexEvent", l)
		}
	})
}

func FuzzTransformPiEvent(f *testing.F) {
	f.Add(`{"type":"user","message":{"content":"hi"}}`)
	f.Add(`{"type":"assistant","parts":[]}`)
	f.Add(`not json`)
	f.Add(``)
	addSeedCorpus(f, "pi", false)
	f.Fuzz(func(t *testing.T, line string) {
		out := transformPiEvent(line)
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			checkTransform(t, "transformPiEvent", l)
		}
	})
}

func FuzzTransformGeminiMessage(f *testing.F) {
	f.Add([]byte(`{"role":"user","parts":[{"text":"hi"}]}`))
	f.Add([]byte(`{"role":"model","parts":[{"functionCall":{"name":"x"}}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	addSeedCorpus(f, "gemini", true)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if !json.Valid(raw) {
			return // transformGeminiMessage is only called on valid JSON entries
		}
		entries := transformGeminiMessage(json.RawMessage(raw), "sess-1")
		for _, e := range entries {
			if _, err := json.Marshal(e); err != nil {
				t.Fatalf("transformGeminiMessage entry not marshalable: %v", err)
			}
		}
	})
}

// --- Unwrapper round-trip oracles ------------------------------------------
//
// The direct fuzzers above prove the unwrappers don't crash on garbage. These
// prove the inverse property: a command put *into* an agent's wrapper comes
// back out intact. The fuzzer drives the inner command; we wrap it the way the
// agent does and assert recovery. This catches the dangerous class of bug
// where the unwrapper silently truncates or mangles the real command — so the
// wrong thing gets gated.
//
// Inputs in each agent's known-lossy subset are skipped with a documented
// guard; that lossiness is intentional (display normalization) and is the
// boundary of what round-trip can assert.

func FuzzUnwrapEvalCommandRoundTrip(f *testing.F) {
	f.Add(`echo hello`)
	f.Add(`git commit -m "fix it"`)
	f.Add(`ls -la /tmp && pwd`)
	f.Add(`echo it's me`)
	f.Fuzz(func(t *testing.T, inner string) {
		// Backslashes hit unwrapEvalCommand's unconditional \" un-escaping,
		// which is intentional display normalization — not round-trippable.
		if strings.ContainsRune(inner, '\\') {
			return
		}
		// The literal end marker inside the command would be cut early.
		if strings.Contains(inner, "' \\< /dev/null") || strings.Contains(inner, `"`) && strings.Contains(inner, "< /dev/null") {
			return
		}
		if inner == "" {
			return
		}
		// Claude single-quotes the command and escapes embedded ' as '"'"'.
		escaped := strings.ReplaceAll(inner, "'", `'"'"'`)
		wrapped := `cd /x && eval '` + escaped + `' \< /dev/null && pwd`
		got := unwrapEvalCommand(wrapped)
		if got != inner {
			t.Fatalf("eval round-trip lost data\n in:  %q\n out: %q", inner, got)
		}
	})
}

func FuzzUnwrapGeminiCommandRoundTrip(f *testing.F) {
	f.Add(`echo hello`)
	f.Add(`git status`)
	f.Add(`for f in *; do echo $f; done`)
	f.Fuzz(func(t *testing.T, inner string) {
		inner = strings.TrimSpace(inner)
		if inner == "" {
			return
		}
		// Gemini's closing token must not appear inside the command, or the
		// unwrapper cuts there.
		if strings.Contains(inner, "};") {
			return
		}
		wrapped := `shopt -u expand_aliases; { ` + inner + ` }; __code=$?; exit $__code`
		got := unwrapGeminiCommand(wrapped)
		if got != inner {
			t.Fatalf("gemini round-trip lost data\n in:  %q\n out: %q", inner, got)
		}
	})
}

func FuzzUnwrapCodexCommandRoundTrip(f *testing.F) {
	f.Add(`echo hello`)
	f.Add(`git status`)
	f.Add(`ls -la | wc -l`)
	f.Fuzz(func(t *testing.T, inner string) {
		if inner == "" {
			return
		}
		// Codex single-quotes the command; unwrapCodexCommand only trims one
		// trailing quote, so embedded single quotes are not round-trippable.
		if strings.ContainsRune(inner, '\'') {
			return
		}
		wrapped := "if . '/x/snap.sh' >/dev/null 2>&1; then :; fi\n" +
			"exec '/bin/zsh' -c '" + inner + "'"
		got := unwrapCodexCommand(wrapped)
		if got != inner {
			t.Fatalf("codex round-trip lost data\n in:  %q\n out: %q", inner, got)
		}
	})
}

// --- isSafeCommand differential oracle -------------------------------------
//
// isSafeCommand decides whether a Bash command skips the permission prompt
// entirely — a security boundary. The invariant: a "safe" command must never
// contain an unquoted output redirect (which could write a file). We check it
// with an independent re-implementation of the quote-aware redirect scan;
// if isSafeCommand and this spec ever disagree, that is the bug.

// hasUnquotedRedirect is the differential oracle: an independent scan for an
// unquoted '>'. Deliberately a separate implementation from isSafeCommand.
func hasUnquotedRedirect(cmd string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '\\' && inDouble && i+1 < len(cmd):
			i++
		case c == '>' && !inSingle && !inDouble:
			return true
		}
	}
	return false
}

func FuzzIsSafeCommand(f *testing.F) {
	f.Add(`ls -la`)
	f.Add(`echo hi > /tmp/x`)
	f.Add(`cat file`)
	f.Add(`rm -rf /`)
	f.Add(`grep 'a > b' file`)
	f.Add(`FOO=bar echo hi`)
	f.Add(`greenlight secrets list`)
	f.Add(`greenlight secrets get token`)
	f.Add(`ls /tmp && rm /tmp/foo`)
	f.Add(`echo hi || rm -rf /`)
	f.Add(`ls | rm -rf /`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, cmd string) {
		safe := isSafeCommand(cmd)
		if safe && hasUnquotedRedirect(cmd) {
			t.Fatalf("isSafeCommand approved a command with an unquoted redirect: %q", cmd)
		}
		// A curated set of file-mutating / code-executing binaries must never
		// be classified safe, regardless of arguments.
		for _, bin := range []string{"rm", "mv", "cp", "dd", "curl", "wget",
			"sh", "bash", "zsh", "python", "python3", "node", "tee", "sed",
			"chmod", "chown", "kill", "git", "ssh"} {
			if isSafeCommand(bin + " " + cmd) {
				t.Fatalf("isSafeCommand approved dangerous binary %q (cmd=%q)", bin, cmd)
			}
			// Dangerous binary in a compound must never be safe either.
			if isSafeCommand("ls /tmp && " + bin + " " + cmd) {
				t.Fatalf("isSafeCommand approved dangerous binary %q in compound (cmd=%q)", bin, cmd)
			}
		}
	})
}

// --- interpose request translation -----------------------------------------
//
// translateInterposeRequest turns a JSON request from the C interpose library
// into a Claude-format tool name + input map. The library is trusted-ish but
// the request still crosses a process boundary; a malformed request must not
// panic and must always yield a known tool name and a JSON-marshalable map.

func FuzzTranslateInterposeRequest(f *testing.F) {
	f.Add([]byte(`{"type":"read","path":"/etc/hosts"}`))
	f.Add([]byte(`{"type":"open","path":"/tmp/x","flags":"w"}`))
	f.Add([]byte(`{"type":"open","path":"/tmp/x","flags":"rename","old_path":"/tmp/y"}`))
	f.Add([]byte(`{"type":"spawn","path":"/bin/sh","args":["sh","-c","echo hi"]}`))
	f.Add([]byte(`{"type":"connect","host":"example.com","port":443}`))
	f.Add([]byte(`{"type":"weird"}`))
	f.Add([]byte(`not json`))
	knownTools := map[string]bool{
		"Read": true, "Write": true, "Edit": true,
		"Bash": true, "WebFetch": true, "Generic": true,
	}
	addSeedCorpus(f, "interpose", true)
	f.Fuzz(func(t *testing.T, raw []byte) {
		var req interposeRequest
		if json.Unmarshal(raw, &req) != nil {
			return // only well-formed requests reach translateInterposeRequest
		}
		tool, input := translateInterposeRequest(req)
		if !knownTools[tool] {
			t.Fatalf("unknown tool name %q for request %q", tool, raw)
		}
		if _, err := json.Marshal(input); err != nil {
			t.Fatalf("translateInterposeRequest input not marshalable: %v", err)
		}
	})
}

// --- crypto: ciphertext parsing --------------------------------------------
//
// decryptSecret parses an attacker-influenceable blob (ephemeral_pub || nonce
// || ciphertext+tag). A truncated or garbage blob must produce a clean error,
// never a panic or an out-of-bounds slice.

func FuzzDecryptSecret(f *testing.F) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		f.Fatalf("generate key: %v", err)
	}
	// A real ciphertext seed so the fuzzer can mutate from valid structure.
	if blob, err := encryptSecret(priv.PublicKey(), []byte("hello")); err == nil {
		f.Add(blob)
	}
	f.Add([]byte{})
	f.Add(make([]byte, 60)) // exactly the minimum length, all zeroes
	f.Add(make([]byte, 59)) // one short
	f.Fuzz(func(t *testing.T, blob []byte) {
		// Must return (plaintext, nil) or (nil, err) — never panic.
		_, _ = decryptSecret(priv, blob)
	})
}

// --- config parsing --------------------------------------------------------
//
// parseConfigValue scans the ~/.greenlight/config file. The file is local and
// low-trust, but a malformed file must not panic; this also exercises the
// scanner's long-line handling.

func FuzzParseConfigValue(f *testing.F) {
	f.Add("device_id=abc\nproject=foo\n", "project")
	f.Add("# comment\n  agent = claude  \n", "agent")
	f.Add("nokey\n=noval\nkey=\n", "key")
	f.Add("", "missing")
	// Project-scoped key form (percent-encoded project segment) must parse like
	// any other key=value line.
	f.Add("project.permit.agent=codex\nproject.org%2Frepo.shim_secret=X\n", "project.permit.agent")
	f.Fuzz(func(t *testing.T, content, key string) {
		_ = parseConfigValue(strings.NewReader(content), key)
	})
}

// --- websocket frame routing -----------------------------------------------
//
// routePermissionResponse decodes server frames. With an empty pending map and
// no control handler it cannot block on a channel send, so it is safe to fuzz
// directly: the property is simply that arbitrary frame bytes never panic.

func FuzzRoutePermissionResponse(f *testing.F) {
	f.Add([]byte(`{"type":"permission_response","request_id":"r1"}`))
	f.Add([]byte(`{"type":"session_started","relay_id":"x"}`))
	f.Add([]byte(`{"type":"wake"}`))
	f.Add([]byte(`{"type":"permission_response"}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`not json at all, padded out past twenty bytes`))
	f.Fuzz(func(t *testing.T, data []byte) {
		c := &WSClient{pending: map[string]chan []byte{}}
		_ = c.routePermissionResponse(data)
	})
}

// --- pure cores extracted from I/O-bound code ------------------------------
//
// computeRenameEdit and resolveScriptCommand both read files before doing the
// interesting work. Their pure cores — diffToEdit and parseShebang — were
// split out specifically so they can be fuzzed without the filesystem.

func FuzzDiffToEdit(f *testing.F) {
	f.Add("line one\nline two\n", "line one\nline TWO\n")
	f.Add("same", "same")
	f.Add("", "added\n")
	f.Add("a\nb\nc", "a\nc")
	f.Add(strings.Repeat("x\n", 5000), strings.Repeat("y\n", 5000))
	f.Fuzz(func(t *testing.T, oldContent, newContent string) {
		oldStr, newStr := diffToEdit(oldContent, newContent)
		// Identical content must produce an empty edit.
		if oldContent == newContent && (oldStr != "" || newStr != "") {
			t.Fatalf("diffToEdit(x, x) returned non-empty edit: %q / %q", oldStr, newStr)
		}
		// The truncation cap is 2048 bytes plus a "\n..." marker.
		const limit = 2048 + len("\n...")
		if len(oldStr) > limit || len(newStr) > limit {
			t.Fatalf("diffToEdit exceeded truncation cap: old=%d new=%d", len(oldStr), len(newStr))
		}
	})
}

// --- transcript transformer preservation oracles ---------------------------
//
// The FuzzTransformXxxEvent fuzzers above only assert "output is valid JSON" —
// which json.Marshal of a map can never violate, so a transformer that drops
// an entire message type passes them. These fuzzers close that gap: they fuzz
// the *content*, wrap it in the agent's event envelope, transform it, and
// assert the content survives into the output. A transformer that drops or
// mangles a recognized message fails here.
//
// Fuzzed text is prefixed with a sentinel so a match cannot collide with a
// role/type field, and is restricted to exclude '<', '>', '*' — the
// characters that trigger the transformers' intentional stripping (Cursor XML
// tags and status suffixes, Codex system-context prefixes).

func jstr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// collectJSONStrings gathers every string value in a decoded JSON document.
func collectJSONStrings(v interface{}, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []interface{}:
		for _, e := range t {
			collectJSONStrings(e, out)
		}
	case map[string]interface{}:
		for _, e := range t {
			collectJSONStrings(e, out)
		}
	}
}

// assertPreserved fails unless `want` appears verbatim as a string value
// somewhere in `output` (which may be one or more newline-separated JSON
// objects). Empty output means the transformer dropped the message entirely.
func assertPreserved(t *testing.T, label, output, want string) {
	t.Helper()
	if strings.TrimSpace(output) == "" {
		t.Fatalf("%s: transformer produced empty output — recognized message dropped", label)
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("%s: output line not JSON: %v", label, err)
		}
		var strs []string
		collectJSONStrings(v, &strs)
		for _, s := range strs {
			if s == want {
				return
			}
		}
	}
	t.Fatalf("%s: input text %q absent from output %q", label, want, output)
}

// skipText reports whether fuzzed text is unsuitable for a preservation
// check: either it would hit a transformer's intentional stripping/skip logic
// ('<', '>', '*'), or it is not valid UTF-8 (transcripts are UTF-8 JSON, and
// json.Marshal would substitute U+FFFD before the transformer ever sees it).
func skipText(raw string) bool {
	return strings.ContainsAny(raw, "<>*") || !utf8.ValidString(raw)
}

func FuzzTransformCopilotRoundTrip(f *testing.F) {
	f.Add("hello world")
	f.Add("multi\nline")
	f.Fuzz(func(t *testing.T, raw string) {
		if skipText(raw) {
			return
		}
		text := "GLFUZZ_" + raw
		assertPreserved(t, "copilot user",
			transformCopilotEvent(`{"type":"user.message","data":{"content":`+jstr(text)+`}}`), text)
		assertPreserved(t, "copilot assistant",
			transformCopilotEvent(`{"type":"assistant.message","data":{"content":`+jstr(text)+`,"model":"m"}}`), text)
	})
}

func FuzzTransformCursorRoundTrip(f *testing.F) {
	f.Add("hello world")
	f.Fuzz(func(t *testing.T, raw string) {
		if skipText(raw) {
			return
		}
		text := "GLFUZZ_" + raw
		part := `{"type":"text","text":` + jstr(text) + `}`
		assertPreserved(t, "cursor user",
			transformCursorEvent(`{"role":"user","message":{"content":[`+part+`]}}`), text)
		assertPreserved(t, "cursor assistant",
			transformCursorEvent(`{"role":"assistant","message":{"content":[`+part+`]}}`), text)
	})
}

func FuzzTransformCodexRoundTrip(f *testing.F) {
	f.Add("hello world")
	f.Fuzz(func(t *testing.T, raw string) {
		if skipText(raw) {
			return
		}
		text := "GLFUZZ_" + raw
		userPart := `{"type":"input_text","text":` + jstr(text) + `}`
		assertPreserved(t, "codex user",
			transformCodexEvent(`{"type":"response_item","payload":{"type":"message","role":"user","content":[`+userPart+`]}}`), text)
		asstPart := `{"type":"output_text","text":` + jstr(text) + `}`
		assertPreserved(t, "codex assistant",
			transformCodexEvent(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[`+asstPart+`]}}`), text)
	})
}

func FuzzTransformPiRoundTrip(f *testing.F) {
	f.Add("hello world")
	f.Fuzz(func(t *testing.T, raw string) {
		if skipText(raw) {
			return
		}
		text := "GLFUZZ_" + raw
		assertPreserved(t, "pi user",
			transformPiEvent(`{"type":"message","message":{"role":"user","content":`+jstr(text)+`}}`), text)
		assertPreserved(t, "pi assistant",
			transformPiEvent(`{"type":"message","message":{"role":"assistant","content":`+jstr(text)+`,"model":"m"}}`), text)
	})
}

func FuzzTransformGeminiRoundTrip(f *testing.F) {
	f.Add("hello world")
	f.Fuzz(func(t *testing.T, raw string) {
		if skipText(raw) {
			return
		}
		text := "GLFUZZ_" + raw
		// transformGeminiMessage returns []map; marshal the slice to reuse
		// the string-walking oracle.
		userEntries := transformGeminiMessage(
			json.RawMessage(`{"type":"user","content":[{"text":`+jstr(text)+`}]}`), "s1")
		userOut, _ := json.Marshal(userEntries)
		assertPreserved(t, "gemini user", string(userOut), text)

		asstEntries := transformGeminiMessage(
			json.RawMessage(`{"type":"gemini","content":`+jstr(text)+`,"model":"m"}`), "s1")
		asstOut, _ := json.Marshal(asstEntries)
		assertPreserved(t, "gemini assistant", string(asstOut), text)
	})
}

func FuzzParseShebang(f *testing.F) {
	f.Add([]byte("#!/bin/sh\n"))
	f.Add([]byte("#!/usr/bin/env node\n"))
	f.Add([]byte("#!/usr/bin/env -S python3 -u\nrest of file"))
	f.Add([]byte("#!   \n"))
	f.Add([]byte("not a script"))
	f.Add([]byte("#!"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, header []byte) {
		interp, _, ok := parseShebang(header)
		if !ok {
			return
		}
		// A successful parse must name a non-empty, single-token interpreter.
		if interp == "" {
			t.Fatalf("parseShebang reported ok with empty interpreter for %q", header)
		}
		if strings.ContainsAny(interp, " \t\n") {
			t.Fatalf("parseShebang interpreter has whitespace: %q (from %q)", interp, header)
		}
		// ok=true requires a real shebang prefix.
		if len(header) < 2 || header[0] != '#' || header[1] != '!' {
			t.Fatalf("parseShebang reported ok for non-shebang %q", header)
		}
	})
}
