//go:build darwin || linux

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestPRBodyClosesIssue(t *testing.T) {
	cases := []struct {
		body string
		id   string
		want bool
	}{
		{"Closes #114", "114", true},
		{"closes #114", "114", true},
		{"fixes #114", "114", true},
		{"Resolved #114", "114", true},
		{"This fixes #114 and more text", "114", true},
		{"Fixes #1, closes #114", "114", true},
		{"line one\nResolves #114\nline three", "114", true},
		{"see #114", "114", false},      // "see" isn't a closing keyword
		{"Closes #1140", "114", false},  // trailing \b stops the prefix match
		{"Closes #11", "114", false},    // different issue
		{"prefixes #114", "114", false}, // "fixes" inside a word
		{"closes#114", "114", false},    // GitHub requires whitespace
		{"no reference at all", "114", false},
		{"Closes #114", "", false}, // empty id never matches
	}
	for _, c := range cases {
		if got := prBodyClosesIssue(c.body, c.id); got != c.want {
			t.Errorf("prBodyClosesIssue(%q, %q) = %v, want %v", c.body, c.id, got, c.want)
		}
	}
}

// ghStub is a configurable GitHub REST stub for the Merge provider tests.
type ghStub struct {
	openPulls []githubPull        // GET /pulls?state=open
	pullByNum map[int]githubPull  // GET /pulls/{n}
	mergeCode int                 // PUT /pulls/{n}/merge status (0 → 200)
	mergeResp githubMergeResponse // PUT body on 200
	issueOpen bool                // GET /issues/{n} state (false → "closed")

	mergedMethod string // captured merge_method from the PUT body
	mergeCalled  bool   // set when the merge PUT is hit
}

func (s *ghStub) serve(t *testing.T) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge"):
			s.mergeCalled = true
			var body struct {
				MergeMethod string `json:"merge_method"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			s.mergedMethod = body.MergeMethod
			if s.mergeCode != 0 && s.mergeCode != 200 {
				w.WriteHeader(s.mergeCode)
				return
			}
			_ = json.NewEncoder(w).Encode(s.mergeResp)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/pulls/"):
			n := pathNum(r.URL.Path, "pulls")
			p, ok := s.pullByNum[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(p)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode(s.openPulls)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/issues/"):
			state := "closed"
			if s.issueOpen {
				state = "open"
			}
			_ = json.NewEncoder(w).Encode(githubIssue{Number: pathNum(r.URL.Path, "issues"), State: state})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	old := githubAPIBase
	githubAPIBase = srv.URL
	return func() { githubAPIBase = old; srv.Close() }
}

// pathNum returns the path segment immediately after `after` as an int.
func pathNum(path, after string) int {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == after && i+1 < len(parts) {
			n, _ := strconv.Atoi(parts[i+1])
			return n
		}
	}
	return 0
}

func TestGithubMerge_AutoResolveSingle(t *testing.T) {
	mergeable := true
	s := &ghStub{
		openPulls: []githubPull{{Number: 117, Body: "Closes #114", Title: "T", HTMLURL: "u"}},
		pullByNum: map[int]githubPull{117: {Number: 117, Title: "T", HTMLURL: "u", State: "open", Body: "Closes #114", Mergeable: &mergeable}},
		mergeResp: githubMergeResponse{SHA: "abc123", Merged: true},
		issueOpen: false, // merge auto-closed it
	}
	defer s.serve(t)()

	res, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.PR != 117 || res.SHA != "abc123" || res.URL != "u" || res.Title != "T" {
		t.Errorf("unexpected result %+v", res)
	}
	if !res.IssueClosed || res.AlreadyMerged {
		t.Errorf("want IssueClosed=true AlreadyMerged=false, got %+v", res)
	}
	if !s.mergeCalled || s.mergedMethod != "merge" {
		t.Errorf("expected merge PUT with method=merge, called=%v method=%q", s.mergeCalled, s.mergedMethod)
	}
}

func TestGithubMerge_MethodPassedThrough(t *testing.T) {
	s := &ghStub{
		openPulls: []githubPull{{Number: 9, Body: "fixes #5", Title: "T", HTMLURL: "u"}},
		pullByNum: map[int]githubPull{9: {Number: 9, State: "open", Body: "fixes #5", Title: "T", HTMLURL: "u"}},
		mergeResp: githubMergeResponse{SHA: "s", Merged: true},
	}
	defer s.serve(t)()

	if _, err := (githubProvider{}).Merge("o", "r", "tok", "5", MergeOptions{Method: "squash"}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if s.mergedMethod != "squash" {
		t.Errorf("merge_method = %q, want squash", s.mergedMethod)
	}
}

func TestGithubMerge_NoLinkedPR(t *testing.T) {
	s := &ghStub{openPulls: []githubPull{{Number: 1, Body: "unrelated work"}}}
	defer s.serve(t)()

	_, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{})
	if err == nil || err.Error() != "no_linked_pr" {
		t.Errorf("err = %v, want no_linked_pr", err)
	}
}

func TestGithubMerge_Ambiguous(t *testing.T) {
	s := &ghStub{openPulls: []githubPull{
		{Number: 1, Body: "Closes #114"},
		{Number: 2, Body: "also closes #114"},
	}}
	defer s.serve(t)()

	_, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{})
	if err == nil || err.Error() != "ambiguous_pr" {
		t.Errorf("err = %v, want ambiguous_pr", err)
	}
}

func TestGithubMerge_NotMergeable405(t *testing.T) {
	s := &ghStub{
		openPulls: []githubPull{{Number: 7, Body: "Closes #114"}},
		pullByNum: map[int]githubPull{7: {Number: 7, State: "open", Body: "Closes #114"}},
		mergeCode: http.StatusMethodNotAllowed,
	}
	defer s.serve(t)()

	_, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{})
	if err == nil || err.Error() != "not_mergeable" {
		t.Errorf("err = %v, want not_mergeable", err)
	}
}

func TestGithubMerge_Conflict409(t *testing.T) {
	s := &ghStub{
		openPulls: []githubPull{{Number: 7, Body: "Closes #114"}},
		pullByNum: map[int]githubPull{7: {Number: 7, State: "open", Body: "Closes #114"}},
		mergeCode: http.StatusConflict,
	}
	defer s.serve(t)()

	_, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{})
	if err == nil || err.Error() != "merge_conflict" {
		t.Errorf("err = %v, want merge_conflict", err)
	}
}

func TestGithubMerge_PreGuardConflict(t *testing.T) {
	mergeable := false
	s := &ghStub{
		pullByNum: map[int]githubPull{7: {Number: 7, State: "open", Mergeable: &mergeable, MergeableState: "dirty"}},
	}
	defer s.serve(t)()

	_, err := githubProvider{}.Merge("o", "r", "tok", "", MergeOptions{PR: 7})
	if err == nil || err.Error() != "merge_conflict" {
		t.Errorf("err = %v, want merge_conflict", err)
	}
	if s.mergeCalled {
		t.Error("merge PUT should not be called when the pre-guard fails")
	}
}

func TestGithubMerge_FreshMergeStillOpen(t *testing.T) {
	// PR merged via --pr but its body had no closing keyword → issue stays open.
	s := &ghStub{
		pullByNum: map[int]githubPull{7: {Number: 7, State: "open", Title: "T", HTMLURL: "u", Body: "no keyword"}},
		mergeResp: githubMergeResponse{SHA: "s", Merged: true},
		issueOpen: true,
	}
	defer s.serve(t)()

	res, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{PR: 7})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.IssueClosed {
		t.Error("want IssueClosed=false for a PR with no closing keyword")
	}
	if res.AlreadyMerged {
		t.Error("want AlreadyMerged=false for a fresh merge")
	}
}

func TestGithubMerge_AlreadyMergedIdempotent(t *testing.T) {
	s := &ghStub{
		pullByNum: map[int]githubPull{7: {Number: 7, State: "closed", Merged: true, Title: "T", HTMLURL: "u", MergeCommitSHA: "old"}},
		issueOpen: false,
	}
	defer s.serve(t)()

	res, err := githubProvider{}.Merge("o", "r", "tok", "114", MergeOptions{PR: 7})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.AlreadyMerged || res.SHA != "old" || !res.IssueClosed {
		t.Errorf("want idempotent already-merged success, got %+v", res)
	}
	if s.mergeCalled {
		t.Error("merge PUT must not be called for an already-merged PR")
	}
}

func TestGithubMerge_PRClosed(t *testing.T) {
	s := &ghStub{
		pullByNum: map[int]githubPull{7: {Number: 7, State: "closed", Merged: false}},
	}
	defer s.serve(t)()

	_, err := githubProvider{}.Merge("o", "r", "tok", "", MergeOptions{PR: 7})
	if err == nil || err.Error() != "pr_closed" {
		t.Errorf("err = %v, want pr_closed", err)
	}
}
