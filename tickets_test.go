//go:build darwin || linux

package main

import "testing"

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"https://github.com/foo/bar.git", "foo", "bar", true},
		{"https://github.com/foo/bar", "foo", "bar", true},
		{"git@github.com:foo/bar.git", "foo", "bar", true},
		{"git@github.com:foo/bar", "foo", "bar", true},
		{"ssh://git@github.com/foo/bar.git", "foo", "bar", true},
		{"https://GitHub.com/Foo/Bar", "Foo", "Bar", true},   // host casing only
		{"https://gitlab.com/foo/bar.git", "", "", false},    // not github
		{"https://github.com/foo", "", "", false},            // missing repo
		{"not a url at all", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := parseGitHubRemote(c.in)
		if ok != c.wantOK || owner != c.wantOwner || repo != c.wantRepo {
			t.Errorf("parseGitHubRemote(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, owner, repo, ok, c.wantOwner, c.wantRepo, c.wantOK)
		}
	}
}

// TestParseIssuesResponse_FiltersPRs guards the gotcha that GitHub's
// /issues endpoint returns pull requests alongside real issues. A PR
// carries a non-null `pull_request` object; an issue's field is absent
// or null. The filter must drop the PR and keep the issue.
func TestParseIssuesResponse_FiltersPRs(t *testing.T) {
	body := []byte(`[
	  {
	    "number": 1,
	    "title": "real issue",
	    "state": "open",
	    "html_url": "https://github.com/foo/bar/issues/1",
	    "updated_at": "2026-05-01T00:00:00Z",
	    "labels": [{"name": "bug"}, {"name": "p1"}]
	  },
	  {
	    "number": 2,
	    "title": "this is actually a PR",
	    "state": "open",
	    "html_url": "https://github.com/foo/bar/pull/2",
	    "updated_at": "2026-05-02T00:00:00Z",
	    "labels": [],
	    "pull_request": {"url": "https://api.github.com/.../pulls/2"}
	  },
	  {
	    "number": 3,
	    "title": "another real issue",
	    "state": "closed",
	    "html_url": "https://github.com/foo/bar/issues/3",
	    "updated_at": "2026-05-03T00:00:00Z",
	    "labels": null,
	    "pull_request": null
	  }
	]`)
	got, err := parseIssuesResponse(body)
	if err != nil {
		t.Fatalf("parseIssuesResponse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tickets, want 2 (PR should be filtered); got=%+v", len(got), got)
	}
	if got[0].Number != 1 || got[1].Number != 3 {
		t.Errorf("kept the wrong items: numbers=%d,%d want 1,3", got[0].Number, got[1].Number)
	}
	if len(got[0].Labels) != 2 || got[0].Labels[0] != "bug" || got[0].Labels[1] != "p1" {
		t.Errorf("labels not flattened: got %v", got[0].Labels)
	}
}

// TestParseIssuesResponse_Empty verifies an empty array yields an empty,
// non-nil slice. tickets_listed always emits `[]` on the wire.
func TestParseIssuesResponse_Empty(t *testing.T) {
	got, err := parseIssuesResponse([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("got nil, want empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("got %d, want 0", len(got))
	}
}
