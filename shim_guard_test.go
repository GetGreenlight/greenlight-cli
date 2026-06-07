//go:build darwin || linux

package main

import (
	"testing"
	"time"
)

func resetShimGuard(t *testing.T) {
	t.Helper()
	shimGuardMu.Lock()
	shimGuard = map[string]time.Time{}
	shimGuardMu.Unlock()
	t.Cleanup(func() {
		shimGuardMu.Lock()
		shimGuard = map[string]time.Time{}
		shimGuardMu.Unlock()
	})
}

func TestShimReexecGuard(t *testing.T) {
	resetShimGuard(t)

	// Nothing recorded yet.
	if consumeShimReexec("/opt/homebrew/bin/gh issue list") {
		t.Fatal("consumed with no recorded entry")
	}

	rememberShimReexec([]string{"gh issue list"})

	// The agent-facing bare form must never consume — only an absolute-path
	// re-exec does. (Otherwise a leftover entry could suppress a fresh prompt.)
	if consumeShimReexec("gh issue list") {
		t.Error("bare command should not consume a guard entry")
	}
	// A non-matching real-binary exec must not consume.
	if consumeShimReexec("/opt/homebrew/bin/gh pr list") {
		t.Error("different command should not consume the entry")
	}
	// The matching absolute-path re-exec consumes once.
	if !consumeShimReexec("/opt/homebrew/bin/gh issue list") {
		t.Error("matching re-exec should consume the entry")
	}
	// Single-use.
	if consumeShimReexec("/opt/homebrew/bin/gh issue list") {
		t.Error("guard entry should be single-use")
	}
}

func TestShimReexecGuard_Expired(t *testing.T) {
	resetShimGuard(t)
	shimGuardMu.Lock()
	shimGuard["gh issue list"] = time.Now().Add(-time.Second) // already expired
	shimGuardMu.Unlock()
	if consumeShimReexec("/usr/bin/gh issue list") {
		t.Error("expired entry should not be consumable")
	}
}

func TestRewriteShimCommandKeys(t *testing.T) {
	setActiveForTest(t, resolvedShim{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"})
	cases := []struct {
		in   string
		keys []string
	}{
		{"gh issue list", []string{"gh issue list"}},
		{"head x | gh issue list", []string{"gh issue list"}}, // per-segment, not the whole pipeline
		{"gh a && gh b", []string{"gh a", "gh b"}},
		{"gh issue list > out.txt", []string{"gh issue list"}}, // redirect excluded from the key
		{"git status", nil}, // no shim → no keys
	}
	for _, c := range cases {
		_, keys := rewriteShimCommandKeys(c.in)
		if len(keys) != len(c.keys) {
			t.Errorf("rewriteShimCommandKeys(%q) keys = %v, want %v", c.in, keys, c.keys)
			continue
		}
		for i := range keys {
			if keys[i] != c.keys[i] {
				t.Errorf("rewriteShimCommandKeys(%q) keys[%d] = %q, want %q", c.in, i, keys[i], c.keys[i])
			}
		}
	}
}
