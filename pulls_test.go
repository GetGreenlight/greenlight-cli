//go:build darwin || linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withFakeGitHub spins up an httptest server, points githubAPIBase at it
// for the duration of the test, and returns the server URL plus a cleanup.
func withFakeGitHub(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Setenv("GREENLIGHT_GITHUB_API_BASE", srv.URL)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCreateGitHubPR_HappyPath(t *testing.T) {
	var gotAuth, gotMethod, gotPath, gotContent string
	var gotBody map[string]string
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContent = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number": 451, "html_url": "https://github.com/foo/bar/pull/451"}`)
	}))

	n, url, err := createGitHubPR("tok", "foo", "bar", "T", "B", "gl/42-x", "main")
	if err != nil {
		t.Fatalf("createGitHubPR: %v", err)
	}
	if n != 451 || url != "https://github.com/foo/bar/pull/451" {
		t.Errorf("got (%d, %q); want (451, https://.../pull/451)", n, url)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotMethod != "POST" || gotPath != "/repos/foo/bar/pulls" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotContent != "application/json" {
		t.Errorf("content-type = %q", gotContent)
	}
	if gotBody["title"] != "T" || gotBody["body"] != "B" || gotBody["head"] != "gl/42-x" || gotBody["base"] != "main" {
		t.Errorf("payload = %+v", gotBody)
	}
}

func TestCreateGitHubPR_ErrorSurface(t *testing.T) {
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"message":"Validation Failed","errors":[]}`)
	}))
	_, _, err := createGitHubPR("tok", "foo", "bar", "T", "B", "br", "main")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("error %q should surface GitHub message", err.Error())
	}
}

func TestMergeGitHubPR_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		fmt.Fprint(w, `{"sha":"abc123","merged":true,"message":"Pull Request successfully merged"}`)
	}))
	sha, err := mergeGitHubPR("tok", "foo", "bar", 451, "squash")
	if err != nil {
		t.Fatalf("mergeGitHubPR: %v", err)
	}
	if sha != "abc123" {
		t.Errorf("sha = %q", sha)
	}
	if gotMethod != "PUT" || gotPath != "/repos/foo/bar/pulls/451/merge" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody["merge_method"] != "squash" {
		t.Errorf("merge_method = %q", gotBody["merge_method"])
	}
}

// 405 is what GitHub returns when a PR is not mergeable (conflicts, draft,
// required checks failing). The handler must surface .message verbatim.
func TestMergeGitHubPR_NotMergeable(t *testing.T) {
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"message":"Branch is not mergeable","documentation_url":"x"}`)
	}))
	_, err := mergeGitHubPR("tok", "foo", "bar", 451, "squash")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Branch is not mergeable") {
		t.Errorf("error %q should surface message", err.Error())
	}
}

// `merged:false` is rare in practice but must still be reported as a failure.
func TestMergeGitHubPR_MergedFalse(t *testing.T) {
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"sha":"abc","merged":false,"message":"already merged"}`)
	}))
	_, err := mergeGitHubPR("tok", "foo", "bar", 1, "squash")
	if err == nil {
		t.Fatal("expected error when merged:false")
	}
}

func TestFetchOpenGitHubPRs(t *testing.T) {
	var gotPath string
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		fmt.Fprint(w, `[
		  {"number":10,"title":"Fixes #1","body":"","html_url":"u10","state":"open","draft":false},
		  {"number":11,"title":"unrelated","body":"resolves #2","html_url":"u11","state":"open","draft":true}
		]`)
	}))
	prs, err := fetchOpenGitHubPRs("tok", "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].Number != 10 || prs[1].Draft != true {
		t.Errorf("prs = %+v", prs)
	}
	if !strings.Contains(gotPath, "/repos/foo/bar/pulls") || !strings.Contains(gotPath, "state=open") {
		t.Errorf("path = %q", gotPath)
	}
}

func TestFetchGitHubIssueTitle(t *testing.T) {
	withFakeGitHub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues/42") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"number":42,"title":"hello world"}`)
	}))
	title, err := fetchGitHubIssueTitle("tok", "foo", "bar", 42)
	if err != nil {
		t.Fatal(err)
	}
	if title != "hello world" {
		t.Errorf("title = %q", title)
	}
}

func TestGitHubErrorMessage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"message":"hi"}`, "hi"},
		{`not json`, "not json"},
		{`{}`, "{}"},
	}
	for _, c := range cases {
		if got := githubErrorMessage([]byte(c.in)); got != c.want {
			t.Errorf("githubErrorMessage(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
