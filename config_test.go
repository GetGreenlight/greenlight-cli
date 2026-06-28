//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempConfig points greenlightDir() at a temp HOME so config read/write
// tests don't touch the real ~/.greenlight/config.
func withTempConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, greenlightDirName())
}

func TestScopedKeyAndPrefix(t *testing.T) {
	if got := scopedKey(scopeHost, "", "agent"); got != "agent" {
		t.Errorf("host scopedKey = %q, want %q", got, "agent")
	}
	// A project name with the delimiter chars must not break the key form.
	for _, proj := range []string{"permit", "a.b", "a/b", "a=b", "a b", "naïve"} {
		prefix := projectKeyPrefix(proj)
		// The encoded project segment must contain none of the delimiter chars,
		// so "project." + enc + "." + key is unambiguous and the key=value line
		// stays parseable.
		enc := strings.TrimSuffix(strings.TrimPrefix(prefix, "project."), ".")
		if strings.ContainsAny(enc, ".= ") {
			t.Errorf("encoded segment %q for project %q contains a delimiter char", enc, proj)
		}
		key := scopedKey(scopeProject, proj, "agent")
		if !strings.HasSuffix(key, ".agent") || strings.Count(key, "=") != 0 {
			t.Errorf("scopedKey(%q) = %q malformed", proj, key)
		}
	}
}

func TestApplyConfigBatchNoClobber(t *testing.T) {
	withTempConfig(t)

	// Seed an out-of-band entry that the UI never knows about.
	if err := applyConfigBatch(scopeHost, "", map[string]string{"tickets_secret": "OOB_SECRET"}, nil); err != nil {
		t.Fatal(err)
	}
	// A separate write of an unrelated key must preserve tickets_secret.
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "codex"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readScoped(scopeHost, "", "tickets_secret"); got != "OOB_SECRET" {
		t.Errorf("tickets_secret clobbered: got %q, want OOB_SECRET", got)
	}
	if got := readScoped(scopeHost, "", "agent"); got != "codex" {
		t.Errorf("agent = %q, want codex", got)
	}
}

func TestWriteConfigValueOnEmptyFile(t *testing.T) {
	withTempConfig(t)
	if err := writeConfigValue("host_id", "host-abc"); err != nil {
		t.Fatal(err)
	}
	path, _ := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "host_id=host-abc\n" {
		t.Errorf("config file = %q, want %q", got, "host_id=host-abc\n")
	}
}

func TestApplyConfigBatchOnEmptyFile(t *testing.T) {
	withTempConfig(t)
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "claude"}, nil); err != nil {
		t.Fatal(err)
	}
	path, _ := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "agent=claude\n" {
		t.Errorf("config file = %q, want %q", got, "agent=claude\n")
	}
}

func TestApplyConfigBatchSetAndUnset(t *testing.T) {
	withTempConfig(t)
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "claude", "shim_secret": "S"}, nil); err != nil {
		t.Fatal(err)
	}
	// Update one and remove the other atomically.
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "gemini"}, []string{"shim_secret"}); err != nil {
		t.Fatal(err)
	}
	if got := readScoped(scopeHost, "", "agent"); got != "gemini" {
		t.Errorf("agent = %q, want gemini", got)
	}
	if got := readScoped(scopeHost, "", "shim_secret"); got != "" {
		t.Errorf("shim_secret = %q, want empty after unset", got)
	}
}

func TestScopeIsolationAndResolution(t *testing.T) {
	withTempConfig(t)
	// Host default + project override for the same key.
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "claude"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigBatch(scopeProject, "permit", map[string]string{"agent": "codex"}, nil); err != nil {
		t.Fatal(err)
	}
	// Host scope is unaffected by the project override.
	if got := readScoped(scopeHost, "", "agent"); got != "claude" {
		t.Errorf("host agent = %q, want claude", got)
	}
	// resolveConfig prefers the project value, falls back to host.
	if got := resolveConfig("permit", "agent"); got != "codex" {
		t.Errorf("resolveConfig(permit, agent) = %q, want codex", got)
	}
	if got := resolveConfig("other", "agent"); got != "claude" {
		t.Errorf("resolveConfig(other, agent) = %q, want claude (host fallback)", got)
	}
}

func TestListScopedExcludesDeviceIDAndProjectKeys(t *testing.T) {
	withTempConfig(t)
	// device_id and host_id are written directly (as register/daemon startup
	// do), not via the batch — both are daemon-internal and must stay hidden.
	if err := writeConfigValue("device_id", "dev-123"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValue("host_id", "host-abc"); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "claude"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigBatch(scopeProject, "permit", map[string]string{"agent": "codex"}, nil); err != nil {
		t.Fatal(err)
	}

	host := listScoped(scopeHost, "")
	if _, ok := host["device_id"]; ok {
		t.Error("listScoped(host) leaked device_id")
	}
	if _, ok := host["host_id"]; ok {
		t.Error("listScoped(host) leaked host_id")
	}
	for k := range host {
		if strings.HasPrefix(k, "project.") {
			t.Errorf("listScoped(host) leaked project key %q", k)
		}
	}
	if host["agent"] != "claude" {
		t.Errorf("listScoped(host)[agent] = %q, want claude", host["agent"])
	}

	proj := listScoped(scopeProject, "permit")
	if proj["agent"] != "codex" {
		t.Errorf("listScoped(project)[agent] = %q, want codex", proj["agent"])
	}
	if len(proj) != 1 {
		t.Errorf("listScoped(project) = %v, want only the agent override", proj)
	}
}

func TestValidateConfigBatch(t *testing.T) {
	cases := []struct {
		name  string
		set   map[string]string
		unset []string
		want  string
	}{
		{"ok agent", map[string]string{"agent": "claude"}, nil, ""},
		{"ok arbitrary", map[string]string{"my_key": "anything"}, nil, ""},
		{"bad agent", map[string]string{"agent": "vim"}, nil, "invalid_agent"},
		{"bad provider", map[string]string{"tickets_provider": "jira"}, nil, "invalid_provider"},
		{"ok provider", map[string]string{"tickets_provider": "github"}, nil, ""},
		{"ok flat chips", map[string]string{"chips": `[{"label":"x","expanded":"y","autosend":true}]`}, nil, ""},
		{"malformed chips", map[string]string{"chips": `[{"label":`}, nil, "invalid_chips"},
		{"device_id set", map[string]string{"device_id": "x"}, nil, "device_id_forbidden"},
		{"device_id unset", nil, []string{"device_id"}, "device_id_forbidden"},
		{"host_id set", map[string]string{"host_id": "x"}, nil, "device_id_forbidden"},
		{"key with equals", map[string]string{"a=b": "v"}, nil, "invalid_key"},
		{"key with space", map[string]string{"a b": "v"}, nil, "invalid_key"},
		{"key leading hash", map[string]string{"#x": "v"}, nil, "invalid_key"},
		{"key project prefix", map[string]string{"project.foo": "v"}, nil, "invalid_key"},
		{"empty key", map[string]string{"": "v"}, nil, "invalid_key"},
		{"unset invalid key", nil, []string{"a=b"}, "invalid_key"},
		{"mid-hash ok", map[string]string{"a#b": "v"}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateConfigBatch(c.set, c.unset); got != c.want {
				t.Errorf("validateConfigBatch() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMigrateLegacyChips(t *testing.T) {
	t.Run("empty value is a no-op", func(t *testing.T) {
		withTempConfig(t)
		migrateLegacyChips()
		if got := readConfigValue(configKeyChips); got != "" {
			t.Errorf("chips = %q, want empty (untouched)", got)
		}
	})

	t.Run("unparseable value is a no-op", func(t *testing.T) {
		withTempConfig(t)
		const garbage = `[{"label":`
		if err := writeConfigValue(configKeyChips, garbage); err != nil {
			t.Fatal(err)
		}
		migrateLegacyChips()
		if got := readConfigValue(configKeyChips); got != garbage {
			t.Errorf("chips = %q, want unchanged %q", got, garbage)
		}
	})

	t.Run("already-flat value is not rewritten", func(t *testing.T) {
		withTempConfig(t)
		// Already normalized — no context markers and no autosend keys. Must be
		// left byte-for-byte intact so a daemon restart can't clobber a user's
		// customised flat set.
		const flat = `[{"label":"Yes","expanded":"Yes"}]`
		if err := writeConfigValue(configKeyChips, flat); err != nil {
			t.Fatal(err)
		}
		migrateLegacyChips()
		if got := readConfigValue(configKeyChips); got != flat {
			t.Errorf("chips = %q, want unchanged %q", got, flat)
		}
	})

	t.Run("context-tagged value is flattened: tickets dropped, fill-only dropped, autosend stripped", func(t *testing.T) {
		withTempConfig(t)
		legacy := `[{"context":"default","label":"Yes","expanded":"Yes","autosend":true},` +
			`{"context":"default","label":"Spec","expanded":"Write a spec for ","autosend":false},` +
			`{"context":"tickets","label":"Work this ticket","expanded":"Work this ticket.","autosend":true}]`
		if err := writeConfigValue(configKeyChips, legacy); err != nil {
			t.Fatal(err)
		}
		migrateLegacyChips()

		got := readConfigValue(configKeyChips)
		// Re-running must be idempotent — the flattened value has no context
		// markers and no autosend keys, so a second pass is a no-op.
		migrateLegacyChips()
		if again := readConfigValue(configKeyChips); again != got {
			t.Errorf("migration not idempotent: first %q, second %q", got, again)
		}

		// The tickets entry is gone, the fill-only "Spec" chip is dropped, and
		// the one surviving "Yes" chip carries neither `context` nor `autosend`.
		var raw []map[string]any
		if err := json.Unmarshal([]byte(got), &raw); err != nil {
			t.Fatalf("migrated chips is not valid JSON: %v (%q)", err, got)
		}
		if len(raw) != 1 {
			t.Fatalf("migrated chips has %d entries, want 1: %q", len(raw), got)
		}
		if raw[0]["label"] != "Yes" || raw[0]["expanded"] != "Yes" {
			t.Errorf("migrated survivor = %v, want {Yes, Yes}", raw[0])
		}
		if _, ok := raw[0]["context"]; ok {
			t.Errorf("migrated chip still carries a context field: %v", raw[0])
		}
		if _, ok := raw[0]["autosend"]; ok {
			t.Errorf("migrated chip still carries an autosend field: %v", raw[0])
		}
	})

	t.Run("flat value with autosend keys: fill-only dropped, survivors stripped, idempotent", func(t *testing.T) {
		withTempConfig(t)
		// Context-free (post-#101) set still carrying the #110 autosend flag:
		// the false entries are dropped, the true survivors lose the key.
		mixed := `[{"label":"Yes","expanded":"Yes","autosend":true},` +
			`{"label":"Subagent","expanded":"Launch a subagent to ","autosend":false},` +
			`{"label":"Continue","expanded":"Continue.","autosend":true}]`
		if err := writeConfigValue(configKeyChips, mixed); err != nil {
			t.Fatal(err)
		}
		migrateLegacyChips()
		got := readConfigValue(configKeyChips)

		var raw []map[string]any
		if err := json.Unmarshal([]byte(got), &raw); err != nil {
			t.Fatalf("migrated chips is not valid JSON: %v (%q)", err, got)
		}
		if len(raw) != 2 {
			t.Fatalf("migrated chips has %d entries, want 2: %q", len(raw), got)
		}
		if raw[0]["label"] != "Yes" || raw[1]["label"] != "Continue" {
			t.Errorf("migrated labels = [%v, %v], want [Yes, Continue]", raw[0]["label"], raw[1]["label"])
		}
		for _, c := range raw {
			if _, ok := c["autosend"]; ok {
				t.Errorf("migrated chip still carries an autosend field: %v", c)
			}
		}

		// Second pass is a no-op — the value is now flat and flagless.
		migrateLegacyChips()
		if again := readConfigValue(configKeyChips); again != got {
			t.Errorf("migration not idempotent: first %q, second %q", got, again)
		}
	})
}

func TestEffectiveConfig(t *testing.T) {
	withTempConfig(t)
	if err := applyConfigBatch(scopeHost, "", map[string]string{"agent": "claude", "tickets_secret": "HOST_TOK"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigBatch(scopeProject, "permit", map[string]string{"agent": "codex"}, nil); err != nil {
		t.Fatal(err)
	}
	// No project → host defaults only.
	host := effectiveConfig("")
	if host["agent"] != "claude" || host["tickets_secret"] != "HOST_TOK" || len(host) != 2 {
		t.Errorf("effectiveConfig(\"\") = %v, want host defaults only", host)
	}
	// With project → host overlaid with project overrides.
	eff := effectiveConfig("permit")
	if eff["agent"] != "codex" {
		t.Errorf("effectiveConfig(permit)[agent] = %q, want codex (override wins)", eff["agent"])
	}
	if eff["tickets_secret"] != "HOST_TOK" {
		t.Errorf("effectiveConfig(permit)[tickets_secret] = %q, want HOST_TOK (inherited)", eff["tickets_secret"])
	}
}

func TestConfiguredShimOverrides(t *testing.T) {
	withTempConfig(t)
	present := map[string]bool{"MY_GH": true, "TS": true}

	// No config → nil (no built-in fallback).
	if got := configuredShimOverrides("permit", present); got != nil {
		t.Errorf("configuredShimOverrides with no config = %v, want nil", got)
	}

	// CLI secret (shim_secret) is preferred when present.
	if err := applyConfigBatch(scopeHost, "", map[string]string{"shim_secret": "MY_GH", "tickets_secret": "TS"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := configuredShimOverrides("permit", present)["gh"]; got != "MY_GH" {
		t.Errorf("configuredShimOverrides gh = %q, want MY_GH (cli secret preferred)", got)
	}

	// CLI secret configured but NOT stored → fall back to the API secret.
	if got := configuredShimOverrides("permit", map[string]bool{"TS": true})["gh"]; got != "TS" {
		t.Errorf("configuredShimOverrides gh = %q, want TS (api-secret fallback when cli secret absent)", got)
	}

	// API secret alone (no shim_secret) feeds the shim.
	withTempConfig(t)
	if err := applyConfigBatch(scopeHost, "", map[string]string{"tickets_secret": "TS"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := configuredShimOverrides("permit", map[string]bool{"TS": true})["gh"]; got != "TS" {
		t.Errorf("configuredShimOverrides gh = %q, want TS (api secret)", got)
	}

	// Configured but secret not stored → nil (do nothing).
	if got := configuredShimOverrides("permit", map[string]bool{"OTHER": true}); got != nil {
		t.Errorf("configuredShimOverrides with unstored secret = %v, want nil", got)
	}
}

// Ensure a project-scoped value round-trips through the on-disk encoding for a
// name containing the delimiter characters.
func TestProjectNameWithSpecialChars(t *testing.T) {
	withTempConfig(t)
	const proj = "org/repo.v2=beta"
	if err := applyConfigBatch(scopeProject, proj, map[string]string{"agent": "pi"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readScoped(scopeProject, proj, "agent"); got != "pi" {
		t.Errorf("round-trip agent = %q, want pi", got)
	}
	// A different project must not see it.
	if got := readScoped(scopeProject, "org/repo", "agent"); got != "" {
		t.Errorf("prefix collision: got %q for sibling project", got)
	}
	// And the on-disk file must remain a flat key=value parseable form.
	path, _ := configPath()
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Errorf("malformed config line: %q", line)
		}
	}
}
