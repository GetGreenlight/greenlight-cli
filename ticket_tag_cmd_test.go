//go:build darwin || linux

package main

import (
	"reflect"
	"testing"
)

func TestSplitTagCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}}, // empty parts dropped
		{"", nil},
		{"   ", nil},
		{"solo", []string{"solo"}},
	}
	for _, tc := range tests {
		got := splitTagCSV(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitTagCSV(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestApplyTagDeltas(t *testing.T) {
	// Output is sorted (server normalizes/dedups authoritatively anyway).
	tests := []struct {
		name   string
		cur    []string
		deltas []string
		want   []string
	}{
		{"add to empty", nil, []string{"+foo", "+bar"}, []string{"bar", "foo"}},
		{"remove existing", []string{"a", "b", "c"}, []string{"-b"}, []string{"a", "c"}},
		{"add and remove", []string{"blocked"}, []string{"+in-progress", "-blocked"}, []string{"in-progress"}},
		{"remove nonexistent is a no-op", []string{"a"}, []string{"-z"}, []string{"a"}},
		{"bare token treated as add", []string{"a"}, []string{"b"}, []string{"a", "b"}},
		{"add already-present is idempotent", []string{"a"}, []string{"+a"}, []string{"a"}},
		{"empty + sign ignored", []string{"a"}, []string{"+"}, []string{"a"}},
		{"remove then re-add", []string{"a"}, []string{"-a", "+a"}, []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyTagDeltas(tc.cur, tc.deltas)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("applyTagDeltas(%v, %v) = %v, want %v", tc.cur, tc.deltas, got, tc.want)
			}
		})
	}
}

func TestDecodeTagsReply(t *testing.T) {
	got := decodeTagsReply([]byte(`{"type":"ticket_tags_set_response","request_id":"r1","tags":["a","b"]}`))
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("decodeTagsReply tags = %v, want [a b]", got)
	}
}
