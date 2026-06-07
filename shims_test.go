//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveShims(t *testing.T) {
	tests := []struct {
		name     string
		present  map[string]bool
		override map[string]string
		want     []resolvedShim
	}{
		{
			name:    "none",
			present: nil,
			want:    nil,
		},
		{
			name:    "no override → no shim (no built-in token fallback)",
			present: map[string]bool{"GITHUB_ACCESS_TOKEN": true, "GITHUB_TOKEN": true},
			want:    nil,
		},
		{
			name:     "override activates when its secret is present",
			present:  map[string]bool{"GITHUB_ACCESS_TOKEN": true},
			override: map[string]string{"gh": "GITHUB_ACCESS_TOKEN"},
			want:     []resolvedShim{{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"}},
		},
		{
			name:     "override with a custom secret name",
			present:  map[string]bool{"MY_GH_PAT": true},
			override: map[string]string{"gh": "MY_GH_PAT"},
			want:     []resolvedShim{{cmd: "gh", secret: "MY_GH_PAT", envName: "GH_TOKEN"}},
		},
		{
			name:     "override ignored when its secret is absent → no shim",
			present:  map[string]bool{"GITHUB_ACCESS_TOKEN": true},
			override: map[string]string{"gh": "MY_GH_PAT"},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activeShims(tt.present, tt.override)
			if len(got) != len(tt.want) {
				t.Fatalf("activeShims() = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("activeShims()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func setActiveForTest(t *testing.T, rs ...resolvedShim) {
	t.Helper()
	activeShimMu.Lock()
	activeShimReg = map[string]resolvedShim{}
	for _, r := range rs {
		activeShimReg[r.cmd] = r
	}
	activeShimMu.Unlock()
	t.Cleanup(func() {
		activeShimMu.Lock()
		activeShimReg = map[string]resolvedShim{}
		activeShimMu.Unlock()
	})
}

func TestRewriteShimCommand(t *testing.T) {
	gh := resolvedShim{cmd: "gh", secret: "GITHUB_ACCESS_TOKEN", envName: "GH_TOKEN"}

	t.Run("no active shims leaves command alone", func(t *testing.T) {
		setActiveForTest(t) // empty
		if got := rewriteShimCommand("gh issue list"); got != "gh issue list" {
			t.Errorf("got %q, want unchanged", got)
		}
	})

	setActiveForTest(t, gh)
	const wrap = "greenlight run -e GH_TOKEN=GITHUB_ACCESS_TOKEN -- "
	cases := []struct {
		in, want string
	}{
		// Simple commands.
		{"gh issue list", wrap + "gh issue list"},
		{"  gh issue view 1  ", "  " + wrap + "gh issue view 1  "}, // surrounding whitespace preserved
		{"gh", wrap + "gh"},
		// Compound: only the gh segment is wrapped, structure preserved.
		{"head x | gh issue list", "head x | " + wrap + "gh issue list"},
		{"gh issue list | head", wrap + "gh issue list | head"},
		{"gh a && gh b", wrap + "gh a && " + wrap + "gh b"},
		{"ls && gh issue list", "ls && " + wrap + "gh issue list"},
		{"gh issue list ; echo done", wrap + "gh issue list ; echo done"},
		{"gh issue list > out.txt", wrap + "gh issue list > out.txt"}, // redirect stays outside the wrap
		// Env-assignment prefix: wrap is inserted after the assignment, which
		// is forwarded correctly through greenlight run.
		{"FOO=bar gh issue list", "FOO=bar " + wrap + "gh issue list"},
		// The parser sees into substitutions, where the inner gh is really run.
		{"echo $(gh issue list)", "echo $(" + wrap + "gh issue list)"},
		{"echo `gh issue list`", "echo `" + wrap + "gh issue list`"},
		// Not rewritten:
		{"/opt/homebrew/bin/gh issue list", "/opt/homebrew/bin/gh issue list"}, // absolute path bypasses shim
		{"git status", "git status"},                                 // unknown command
		{"glab mr list", "glab mr list"},                             // not active in this set
		{"echo 'gh issue list' | cat", "echo 'gh issue list' | cat"}, // shim name only inside a quote
		{"echo gh", "echo gh"},                                       // gh as an argument, not the command
		{"", ""},
	}
	for _, c := range cases {
		if got := rewriteShimCommand(c.in); got != c.want {
			t.Errorf("rewriteShimCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLookupActiveShim guards the trust-model fix: the daemon answers a shim's
// resolve_shim request from the same activeShimReg entry that drives the phone
// display, so runShim injects exactly the secret that was approved.
func TestLookupActiveShim(t *testing.T) {
	gh := resolvedShim{cmd: "gh", secret: "GITHUB_TOKEN", envName: "GH_TOKEN"}
	setActiveForTest(t, gh)

	got, ok := lookupActiveShim("gh")
	if !ok || got != gh {
		t.Errorf("lookupActiveShim(gh) = %+v, %v; want %+v, true", got, ok, gh)
	}
	if _, ok := lookupActiveShim("glab"); ok {
		t.Error("lookupActiveShim(glab) returned active for an unconfigured shim")
	}
}

func TestResolveRealBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	tmp := t.TempDir()
	shimDir := filepath.Join(tmp, "shim")
	realDir := filepath.Join(tmp, "real")
	if err := os.MkdirAll(shimDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Shim "gh" is a symlink back to the running (test) binary.
	if err := os.Symlink(self, filepath.Join(shimDir, "gh")); err != nil {
		t.Fatal(err)
	}
	// Real "gh" is a distinct executable file.
	realGh := filepath.Join(realDir, "gh")
	if err := os.WriteFile(realGh, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("skips the shim symlink and finds the real binary", func(t *testing.T) {
		t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)
		got, err := resolveRealBinary("gh")
		if err != nil {
			t.Fatalf("resolveRealBinary: %v", err)
		}
		want, _ := filepath.EvalSymlinks(realGh)
		gotResolved, _ := filepath.EvalSymlinks(got)
		if gotResolved != want {
			t.Errorf("resolveRealBinary = %q (resolved %q), want %q", got, gotResolved, want)
		}
	})

	t.Run("returns not-found when only the shim is on PATH", func(t *testing.T) {
		t.Setenv("PATH", shimDir)
		if _, err := resolveRealBinary("gh"); err == nil {
			t.Errorf("expected not-found error, got nil")
		}
	})
}
