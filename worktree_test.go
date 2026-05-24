//go:build darwin || linux

package main

import (
	"reflect"
	"testing"
)

func TestParseTicketRef(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantN     int
		wantOK    bool
	}{
		{"github:foo/bar#42", "foo", "bar", 42, true},
		{"github:Foo/Bar-Baz#1", "Foo", "Bar-Baz", 1, true},
		{"github:foo/bar#0", "", "", 0, false},     // 0 isn't a valid issue
		{"github:foo/bar", "", "", 0, false},       // no #N
		{"foo/bar#1", "", "", 0, false},            // missing scheme
		{"github:foo#1", "", "", 0, false},         // no /repo
		{"", "", "", 0, false},
	}
	for _, c := range cases {
		owner, repo, n, ok := parseTicketRef(c.in)
		if ok != c.wantOK || owner != c.wantOwner || repo != c.wantRepo || n != c.wantN {
			t.Errorf("parseTicketRef(%q) = (%q, %q, %d, %v); want (%q, %q, %d, %v)",
				c.in, owner, repo, n, ok, c.wantOwner, c.wantRepo, c.wantN, c.wantOK)
		}
	}
}

func TestSlugifyTitle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Add a thing", "add-a-thing"},
		{"  Add a thing  ", "add-a-thing"},
		{"Fix bug! (issue #1)", "fix-bug-issue-1"},
		{"---weird---", "weird"},
		{"", ""},
		{"!@#$%^&*()", ""},
		// length cap: 40 chars, cut at dash boundary
		{
			in:   "this is a much longer title that should be cut off well before forty characters",
			want: "this-is-a-much-longer-title-that-should",
		},
	}
	for _, c := range cases {
		got := slugifyTitle(c.in)
		if got != c.want {
			t.Errorf("slugifyTitle(%q) = %q; want %q", c.in, got, c.want)
		}
		if len(got) > 40 {
			t.Errorf("slugifyTitle(%q) = %q (len %d > 40)", c.in, got, len(got))
		}
	}
}

func TestBranchNameForTicket(t *testing.T) {
	cases := []struct {
		number int
		title  string
		want   string
	}{
		{42, "Add a thing", "gl/42-add-a-thing"},
		{1, "", "gl/1"},
		{1, "!@#", "gl/1"}, // slug empties out
		{17, "Fix #2", "gl/17-fix-2"},
	}
	for _, c := range cases {
		got := branchNameForTicket(c.number, c.title)
		if got != c.want {
			t.Errorf("branchNameForTicket(%d, %q) = %q; want %q", c.number, c.title, got, c.want)
		}
	}
}

func TestIssueNumberFromBranch(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"gl/42-add-a-thing", 42},
		{"gl/1", 1},
		{"gl/", 0},
		{"foo/123-bar", 0},
		{"main", 0},
		{"gl/42x-bad", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := issueNumberFromBranch(c.in)
		if got != c.want {
			t.Errorf("issueNumberFromBranch(%q) = %d; want %d", c.in, got, c.want)
		}
	}
}

func TestParseClosingKeywords(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"Fixes #42", []int{42}},
		{"Fixes #1, closes #2, resolved #3", []int{1, 2, 3}},
		{"fix #10\ncloses #20", []int{10, 20}},
		{"Closed #7", []int{7}},
		{"Resolves: #5", nil}, // colon between keyword and # isn't a match
		{"Fixed #99 (also see #100)", []int{99}}, // bare #100 isn't a closing kw
		{"FIXES #42", []int{42}}, // case-insensitive
		{"fixesn #42", nil}, // not a word boundary
		{"This fixes the bug in #42", nil}, // no closing keyword adjacent
		{"closes#42", nil}, // needs whitespace
		{"", nil},
		// dedupe: same number twice → only once
		{"fixes #1, fixes #1", []int{1}},
	}
	for _, c := range cases {
		got := parseClosingKeywords(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseClosingKeywords(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestAttachLinkedPRs(t *testing.T) {
	items := []ticket{
		{Number: 1, Title: "one"},
		{Number: 2, Title: "two"},
		{Number: 3, Title: "three"},
	}
	prs := []rawPR{
		{Number: 100, Title: "do the thing", Body: "Fixes #1", HTMLURL: "u100", State: "open", Draft: false},
		{Number: 101, Title: "Closes #2 (draft)", Body: "", HTMLURL: "u101", State: "open", Draft: true},
		{Number: 102, Title: "Closes #2 (non-draft)", Body: "", HTMLURL: "u102", State: "open", Draft: false},
		// PR references an issue not in the list; ignored.
		{Number: 103, Title: "Fixes #999", Body: "", HTMLURL: "u103", State: "open"},
	}

	attachLinkedPRs(items, prs)

	if items[0].LinkedPR == nil || items[0].LinkedPR.Number != 100 {
		t.Errorf("#1 should be linked to PR 100; got %+v", items[0].LinkedPR)
	}
	if items[1].LinkedPR == nil || items[1].LinkedPR.Number != 102 {
		t.Errorf("#2 should prefer non-draft PR 102 over draft 101; got %+v", items[1].LinkedPR)
	}
	if items[2].LinkedPR != nil {
		t.Errorf("#3 should have no linked_pr; got %+v", items[2].LinkedPR)
	}
}

// Once a non-draft PR is attached, a subsequent draft PR for the same issue
// shouldn't overwrite it.
func TestAttachLinkedPRs_NonDraftSticky(t *testing.T) {
	items := []ticket{{Number: 5}}
	prs := []rawPR{
		{Number: 50, Title: "Fixes #5", State: "open", Draft: false, HTMLURL: "u50"},
		{Number: 51, Title: "Fixes #5", State: "open", Draft: true, HTMLURL: "u51"},
	}
	attachLinkedPRs(items, prs)
	if items[0].LinkedPR == nil || items[0].LinkedPR.Number != 50 {
		t.Fatalf("expected PR 50 (non-draft) to stick; got %+v", items[0].LinkedPR)
	}
}
