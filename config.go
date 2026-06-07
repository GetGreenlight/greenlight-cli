//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Well-known config keys. Arbitrary keys are also allowed (free strings); only
// these have special handling (enum validation or exclusion from interfaces).
const (
	configKeyDeviceID        = "device_id"        // daemon-internal; never exposed via config get/set/list
	configKeyHostID          = "host_id"          // daemon-internal (persisted host UUID); never exposed
	configKeyAgent           = "agent"            // enum: knownAgents
	configKeyTicketsProvider = "tickets_provider" // enum: knownTicketProviders
	configKeyTicketsSecret   = "tickets_secret"   // greenlight secret name for the provider API token
	configKeyShimSecret      = "shim_secret"      // greenlight secret name injected into the shimmed provider CLI
	configKeyChips           = "chips"            // JSON array of prompt chip rules (see configChip)
	configKeyIdleNotifyAfter = "idle_notify_after" // duration after which to send idle push notification (e.g. "5m", "1h")
)

// configChip is one row in the `chips` config array. context must be "default"
// or "tickets". autosend=true sends the expanded text immediately; false fills
// the input field and focuses it.
type configChip struct {
	Context  string `json:"context"`
	Label    string `json:"label"`
	Expanded string `json:"expanded"`
	AutoSend bool   `json:"autosend"`
}

// defaultChipsJSON is the compact JSON written to config on first daemon start.
const defaultChipsJSON = `[{"context":"default","label":"Yes","expanded":"Yes","autosend":true},{"context":"default","label":"Continue","expanded":"Continue.","autosend":true},{"context":"default","label":"Subagent","expanded":"Launch a subagent to ","autosend":false},{"context":"default","label":"Commit & push","expanded":"Commit the current changes with a descriptive message, then push.","autosend":false},{"context":"default","label":"Spec","expanded":"Write a spec for ","autosend":false},{"context":"tickets","label":"Yes","expanded":"Yes","autosend":true},{"context":"tickets","label":"Subagent","expanded":"Launch a subagent to ","autosend":false},{"context":"tickets","label":"Work this ticket","expanded":"Work this ticket. Read it carefully to understand the acceptance criteria, implement the change on a new branch off main, and open a PR that closes it.","autosend":true},{"context":"tickets","label":"Review the PR","expanded":"Find the open PR for this ticket and review it. Leave review comments with anything that needs to change.","autosend":true},{"context":"tickets","label":"Update the spec","expanded":"Update this ticket's description to reflect the latest decisions.","autosend":true}]`

// reservedConfigKeys are daemon-internal keys (written by register/daemon
// startup) that must never surface in or be writable through the config
// interface — they'd corrupt device/host identity.
var reservedConfigKeys = map[string]bool{
	configKeyDeviceID: true,
	configKeyHostID:   true,
}

// Config scopes. "host" keys are stored bare; "project" keys are stored under a
// percent-encoded prefix (see projectKeyPrefix). The on-disk file stays flat —
// the host/project distinction is reconstructed from the key, not stored as
// structure.
const (
	scopeHost    = "host"
	scopeProject = "project"
)

// defaultTicketsProvider is the provider the command shim assumes when a
// shim_secret is configured but tickets_provider isn't (github → the `gh` CLI).
// The tickets *display* feature has no default — it stays off until configured.
const defaultTicketsProvider = "github"

// projectKeyPrefix returns the on-disk key prefix for a project's scoped
// entries: "project.<enc>." where <enc> is the percent-encoded project name.
// We percent-encode (and additionally escape '.') so that '.', '/', '=' or
// whitespace in a project name can never collide with the key delimiters or the
// key=value separator — the bare key after the prefix is then unambiguous.
func projectKeyPrefix(project string) string {
	enc := strings.ReplaceAll(url.QueryEscape(project), ".", "%2E")
	return "project." + enc + "."
}

// scopedKey returns the on-disk key for (scope, project, key). Host scope is the
// bare key; project scope is prefixed.
func scopedKey(scope, project, key string) string {
	if scope == scopeProject {
		return projectKeyPrefix(project) + key
	}
	return key
}

// readScoped reads a single key at the given scope. Project scope requires a
// non-empty project.
func readScoped(scope, project, key string) string {
	return readConfigValue(scopedKey(scope, project, key))
}

// resolveConfig resolves a key with project-override-then-host-then-empty
// precedence. Used by config consumers (agent/tickets/shims) so a project entry
// shadows the host default.
func resolveConfig(project, key string) string {
	if project != "" {
		if v := readScoped(scopeProject, project, key); v != "" {
			return v
		}
	}
	return readScoped(scopeHost, "", key)
}

// readAllConfig parses the whole config file into a key→value map. Blank lines
// and #-comments are skipped. Returns an empty map if the file is absent.
func readAllConfig() map[string]string {
	out := map[string]string{}
	path, err := configPath()
	if err != nil {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// listScoped returns the bare key→value entries stored at the given scope.
// Host scope excludes project-prefixed keys and device_id. Project scope returns
// only that project's entries with the prefix stripped.
func listScoped(scope, project string) map[string]string {
	all := readAllConfig()
	out := map[string]string{}
	if scope == scopeProject {
		prefix := projectKeyPrefix(project)
		for k, v := range all {
			if strings.HasPrefix(k, prefix) {
				out[strings.TrimPrefix(k, prefix)] = v
			}
		}
		return out
	}
	for k, v := range all {
		if strings.HasPrefix(k, "project.") || reservedConfigKeys[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// validateConfigBatch checks a set/unset batch before any write. It rejects
// device_id at either scope and validates the enum-constrained well-known keys.
// All other keys accept any string. Returns a stable wire error code, or "" if
// the batch is valid.
func validateConfigBatch(set map[string]string, unset []string) string {
	for _, k := range unset {
		if reservedConfigKeys[k] {
			return "device_id_forbidden"
		}
		if !validConfigKey(k) {
			return "invalid_key"
		}
	}
	for k, v := range set {
		if reservedConfigKeys[k] {
			return "device_id_forbidden"
		}
		if !validConfigKey(k) {
			return "invalid_key"
		}
		switch k {
		case configKeyAgent:
			if !knownAgents[v] {
				return "invalid_agent"
			}
		case configKeyTicketsProvider:
			if !knownTicketProviders[v] {
				return "invalid_provider"
			}
		case configKeyChips:
			var chips []configChip
			if json.Unmarshal([]byte(v), &chips) != nil {
				return "invalid_chips"
			}
			for _, c := range chips {
				if c.Context != "default" && c.Context != "tickets" {
					return "invalid_chips"
				}
			}
		case configKeyIdleNotifyAfter:
			if v != "" && v != "0" {
				d, err := time.ParseDuration(v)
				if err != nil || d < time.Minute {
					return "invalid_value"
				}
			}
		}
	}
	return ""
}

// validConfigKey rejects keys that would corrupt the flat key=value file or
// collide with the reserved project namespace: a key must be non-empty, contain
// no '=' (the line separator) or whitespace, not start with '#' (parsed as a
// comment), and not use the reserved "project." prefix (that's how project
// overrides are stored — a user key with it would become a phantom override
// hidden from the host listing). Mirrored in the app pickers, but enforced here
// since the CLI is the authoritative validator for free-form / wire callers.
func validConfigKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "#") || strings.HasPrefix(key, "project.") {
		return false
	}
	return !strings.ContainsAny(key, "= \t\r\n")
}

// effectiveConfig returns the resolved key→value view for a project: the host
// defaults overlaid with that project's overrides. With an empty project it is
// just the host defaults. device_id is excluded (listScoped drops it). This is
// the human-facing "effective view"; the daemon config_get path keeps host and
// project separate so the apps can mark which keys are overridden.
func effectiveConfig(project string) map[string]string {
	out := listScoped(scopeHost, "")
	if project != "" {
		for k, v := range listScoped(scopeProject, project) {
			out[k] = v
		}
	}
	return out
}

// applyConfigBatch atomically applies a set of upserts and a list of deletes at
// the given scope in a single read-modify-write of the config file. Only the
// named keys are touched — every other entry is preserved verbatim, so an
// out-of-band edit (CLI or another writer) of an unrelated key is never
// clobbered. set is applied before unset; a key in both is deleted.
func applyConfigBatch(scope, project string, set map[string]string, unset []string) error {
	path, err := configPath()
	if err != nil {
		return fmt.Errorf("cannot determine config path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create config dir: %w", err)
	}

	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
	}

	setKeys := make(map[string]string, len(set))
	for k, v := range set {
		setKeys[scopedKey(scope, project, k)] = v
	}
	unsetKeys := make(map[string]bool, len(unset))
	for _, k := range unset {
		unsetKeys[scopedKey(scope, project, k)] = true
	}

	out := make([]string, 0, len(lines))
	applied := make(map[string]bool)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		if k, _, ok := strings.Cut(trimmed, "="); ok {
			key := strings.TrimSpace(k)
			if unsetKeys[key] {
				continue // drop
			}
			if v, found := setKeys[key]; found {
				out = append(out, key+"="+v)
				applied[key] = true
				continue
			}
		}
		out = append(out, line)
	}

	// Append set keys that weren't already present, in deterministic order.
	var pending []string
	for key := range setKeys {
		if !applied[key] {
			pending = append(pending, key)
		}
	}
	sort.Strings(pending)

	// Trim trailing blank lines before appending new keys, so repeated in-place
	// edits don't accumulate them (ReadFile's trailing newline splits into a
	// final "" element each time) and new keys attach directly after content.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for _, key := range pending {
		out = append(out, key+"="+setKeys[key])
	}

	if len(out) == 0 {
		return os.WriteFile(path, []byte{}, 0644)
	}
	output := strings.Join(out, "\n") + "\n"
	return os.WriteFile(path, []byte(output), 0644)
}

// configPath returns the path to the greenlight config file.
func configPath() (string, error) {
	dir, err := greenlightDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// readConfigValue reads a value by key from ~/.greenlight/config.
// The config file uses simple key=value format, one per line.
// Returns empty string if the file doesn't exist or the key is not found.
func readConfigValue(key string) string {
	path, err := configPath()
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseConfigValue(f, key)
}

// parseConfigValue is the pure parsing core of readConfigValue: it scans
// key=value lines from r and returns the value for key, or "" if absent.
// Blank lines and #-comments are skipped. Separated out so it can be fuzzed
// without touching the filesystem.
func parseConfigValue(r io.Reader, key string) string {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeConfigValue upserts a key=value pair in ~/.greenlight/config,
// preserving all other entries.
func writeConfigValue(key, value string) error {
	path, err := configPath()
	if err != nil {
		return fmt.Errorf("cannot determine config path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create config dir: %w", err)
	}

	// Read existing lines
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}

	// Upsert the key
	found := false
	entry := key + "=" + value
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, _, ok := strings.Cut(trimmed, "=")
		if ok && strings.TrimSpace(k) == key {
			lines[i] = entry
			found = true
			break
		}
	}
	// Remove trailing empty lines before writing.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if !found {
		lines = append(lines, entry)
	}

	if len(lines) == 0 {
		return os.WriteFile(path, []byte{}, 0644)
	}
	output := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(output), 0644)
}
