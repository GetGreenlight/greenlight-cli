//go:build darwin || linux

package main

import (
	"encoding/json"
	"testing"
)

// TestSessionRecord_TicketRoundTrip guards the on-disk schema: a ticket
// field must survive marshal/unmarshal so resume can re-export
// GREENLIGHT_TICKET from a persisted record. The omitempty tag also means
// records written before ticket support stay parseable.
func TestSessionRecord_TicketRoundTrip(t *testing.T) {
	in := sessionRecord{
		ConversationID: "conv-1",
		RelayID:        "relay-1",
		Agent:          "claude",
		Project:        "proj",
		Ticket:         "github:foo/bar#423",
		Cwd:            "/tmp/x",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out sessionRecord
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Ticket != in.Ticket {
		t.Errorf("ticket roundtrip: got %q want %q", out.Ticket, in.Ticket)
	}

	// Records written before ticket support omit the field. They must still
	// parse, with Ticket defaulting to "".
	legacy := []byte(`{"conversation_id":"c","relay_id":"r","agent":"claude","project":"p","cwd":"/tmp"}`)
	var legacyRec sessionRecord
	if err := json.Unmarshal(legacy, &legacyRec); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacyRec.Ticket != "" {
		t.Errorf("legacy ticket: got %q want empty", legacyRec.Ticket)
	}

	// omitempty: empty ticket should not appear in the serialized form, so
	// records keep their pre-ticket shape on disk when no ticket is set.
	empty, _ := json.Marshal(sessionRecord{ConversationID: "c"})
	if got := string(empty); jsonContains(got, "ticket") {
		t.Errorf("empty ticket should be omitted, got %s", got)
	}
}

func jsonContains(s, key string) bool {
	return containsKey([]byte(s), key)
}

func containsKey(data []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
