//go:build darwin || linux

package main

import "testing"

func TestStageStart(t *testing.T) {
	tests := []struct {
		cur, want string
	}{
		{"", "spec-in-progress"},                 // no stage → first phase, in-progress
		{"spec-needed", "spec-in-progress"},      // advances
		{"code-needed", "code-in-progress"},      // advances, preserves phase
		{"spec-in-progress", "spec-in-progress"}, // idempotent
		{"spec-in-review", "spec-in-review"},     // gate: no cross past review
		{"code-in-review", "code-in-review"},     // done-ish: unchanged
	}
	for _, tc := range tests {
		if got := stageStart(tc.cur); got != tc.want {
			t.Errorf("stageStart(%q) = %q, want %q", tc.cur, got, tc.want)
		}
	}
}

func TestStageSubmit(t *testing.T) {
	tests := []struct {
		cur, want string
	}{
		{"", "spec-in-review"},                 // no stage → first phase, in-review
		{"spec-needed", "spec-in-review"},      // skipped start → straight to review
		{"spec-in-progress", "spec-in-review"}, // advances
		{"code-in-progress", "code-in-review"}, // advances, preserves phase
		{"spec-in-review", "spec-in-review"},   // idempotent
		{"code-in-review", "code-in-review"},   // unchanged
	}
	for _, tc := range tests {
		if got := stageSubmit(tc.cur); got != tc.want {
			t.Errorf("stageSubmit(%q) = %q, want %q", tc.cur, got, tc.want)
		}
	}
}

func TestStageApprove(t *testing.T) {
	tests := []struct {
		cur, want string
	}{
		{"spec-in-review", "code-needed"},        // crosses the gate to the code phase
		{"code-in-review", "code-in-review"},     // final review: no further stage, no auto-close
		{"spec-needed", "spec-needed"},           // not in review: no-op
		{"spec-in-progress", "spec-in-progress"}, // not in review: no-op
		{"code-needed", "code-needed"},           // not in review: no-op
		{"", ""},                                 // no stage: no-op
	}
	for _, tc := range tests {
		if got := stageApprove(tc.cur); got != tc.want {
			t.Errorf("stageApprove(%q) = %q, want %q", tc.cur, got, tc.want)
		}
	}
}

func TestStageReject(t *testing.T) {
	tests := []struct {
		cur, want string
	}{
		{"spec-in-review", "spec-in-progress"},   // back for rework, same phase
		{"code-in-review", "code-in-progress"},   // back for rework, same phase
		{"spec-needed", "spec-needed"},           // not in review: no-op
		{"code-in-progress", "code-in-progress"}, // not in review: no-op
		{"", ""},                                 // no stage: no-op
	}
	for _, tc := range tests {
		if got := stageReject(tc.cur); got != tc.want {
			t.Errorf("stageReject(%q) = %q, want %q", tc.cur, got, tc.want)
		}
	}
}

// Re-applying a verb to its own output must be a no-op (idempotent), so an
// agent retry after a flaky-looking success can't double-advance.
func TestStageMoveIdempotent(t *testing.T) {
	for _, start := range []string{"", "spec-needed", "code-needed", "spec-in-progress", "code-in-progress", "spec-in-review", "code-in-review"} {
		s1 := stageStart(start)
		if s2 := stageStart(s1); s2 != s1 {
			t.Errorf("stageStart not idempotent: %q → %q → %q", start, s1, s2)
		}
		u1 := stageSubmit(start)
		if u2 := stageSubmit(u1); u2 != u1 {
			t.Errorf("stageSubmit not idempotent: %q → %q → %q", start, u1, u2)
		}
		a1 := stageApprove(start)
		if a2 := stageApprove(a1); a2 != a1 {
			t.Errorf("stageApprove not idempotent: %q → %q → %q", start, a1, a2)
		}
		r1 := stageReject(start)
		if r2 := stageReject(r1); r2 != r1 {
			t.Errorf("stageReject not idempotent: %q → %q → %q", start, r1, r2)
		}
	}
}
