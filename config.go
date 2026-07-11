//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	configKeyDeviceID        = "device_id"         // daemon-internal; never exposed via config get/set/list
	configKeyHostID          = "host_id"           // daemon-internal (persisted host UUID); never exposed
	configKeyAgent           = "agent"             // enum: knownAgents
	configKeyTicketsProvider = "tickets_provider"  // enum: knownTicketProviders
	configKeyTicketsSecret   = "tickets_secret"    // greenlight secret name for the provider API token
	configKeyShimSecret      = "shim_secret"       // greenlight secret name injected into the shimmed provider CLI
	configKeyChips           = "chips"             // JSON array of prompt chip rules (see configChip)
	configKeyIdleNotifyAfter = "idle_notify_after" // duration after which to send idle push notification (e.g. "5m", "1h")
	configKeyScratchAuto     = "scratch_auto"      // bool (default on): report OS scratch dirs as ephemeral trusted roots (#119)
	configKeySSHIsolation    = "ssh_isolation"     // bool (default off): strip inherited SSH_AUTH_SOCK/SSH_AGENT_PID from the child env (#249)
	configKeySSHKeys         = "ssh_keys"          // comma-separated secret names (SSH_KEY_*) the session ssh-agent serves; only consulted when ssh_isolation=on (#250)
)

// configChip is one row in the `chips` config array — a single flat set applied
// to every session (ticket-backed or not). Every chip sends its expanded text
// immediately on tap (#110 removed the former fill-only `autosend` flag). Ticket
// sessions get their stage-aware and close/re-open chips from runtime logic in
// the apps, not from this config (see #101 — the former per-context chip sets
// were removed).
type configChip struct {
	Label    string `json:"label"`
	Expanded string `json:"expanded"`
}

// defaultChipsJSON is the compact JSON written to config on first daemon start.
// Only the two always-send defaults remain; #110 dropped the prefix-style
// fill-only chips (Subagent/Commit & push/Spec) since auto-sending a template is
// nonsense.
const defaultChipsJSON = `[{"label":"Yes","expanded":"Yes"},{"label":"Continue","expanded":"Continue."}]`

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

// scratchAutoEnabled resolves the scratch_auto config knob (#119). Defaults to
// true (scratch dirs are reported as ephemeral trusted roots); a paranoid user
// sets it false (off/0/no/false) to keep tmp prompts.
func scratchAutoEnabled(project string) bool {
	v := strings.ToLower(strings.TrimSpace(resolveConfig(project, configKeyScratchAuto)))
	switch v {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// sshIsolationEnabled resolves the ssh_isolation config knob (#249). Mirrors
// scratchAutoEnabled but defaults FALSE: isolation is opt-in, so only an
// explicit truthy value (1/true/on/yes) turns it on — empty/unset or anything
// else leaves the child env passthrough behavior unchanged.
func sshIsolationEnabled(project string) bool {
	v := strings.ToLower(strings.TrimSpace(resolveConfig(project, configKeySSHIsolation)))
	switch v {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// validSSHIsolationValues are the accepted spellings for the ssh_isolation
// bool, checked case-insensitively after trim. Stricter than scratch_auto
// (which coerces any string) because the default is off — a typo like "onn"
// must be rejected loudly rather than silently leaving isolation disabled.
var validSSHIsolationValues = map[string]bool{
	"on": true, "off": true, "true": true, "false": true,
	"1": true, "0": true, "yes": true, "no": true,
}

// validSSHKeysValue validates an ssh_keys config value (#250): empty (no keys)
// or a comma-separated list where every trimmed entry is a valid secret key
// name. Existence is deliberately NOT checked — the secret may be created
// after the config is written; an unresolvable entry at session start is
// skipped with a log line instead (see resolveSSHSession).
func validSSHKeysValue(v string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	for _, part := range strings.Split(v, ",") {
		if !validSecretKey(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

// sshConfiguredKeys parses the resolved ssh_keys value for a project into the
// list of configured secret names, dropping empty entries. Only meaningful
// when ssh_isolation is on; callers gate on that.
func sshConfiguredKeys(project string) []string {
	v := resolveConfig(project, configKeySSHKeys)
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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
		case configKeyIdleNotifyAfter:
			if v != "" && v != "0" {
				d, err := time.ParseDuration(v)
				if err != nil || d < time.Minute {
					return "invalid_value"
				}
			}
		case configKeySSHIsolation:
			if !validSSHIsolationValues[strings.ToLower(strings.TrimSpace(v))] {
				return "invalid_value"
			}
		case configKeySSHKeys:
			if !validSSHKeysValue(v) {
				return "invalid_value"
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

// migrateLegacyChips normalizes the host-scope `chips` config to the flat,
// flagless single-set form `[{label, expanded}, ...]`. It folds in two
// historical migrations:
//   - pre-#101: drops the "tickets"-context entries (superseded by runtime
//     stage chips) and keeps the "default"/untagged ones, dropping the
//     now-removed `context` field.
//   - #110: drops fill-only chips (`autosend: false` — templates meant to be
//     edited before sending, nonsense to auto-send) and strips the now-removed
//     `autosend` key from the survivors.
//
// The keep rule is "keep unless autosend is explicitly false", so a key-less
// entry (an already-migrated survivor) is preserved. A config is considered
// already normalized — and left byte-for-byte untouched — only when every entry
// is exactly {label, expanded} with no `context` and no `autosend` keys; that
// exact shape is the one daemon-restart no-op, so this runs once per install.
// Persists via the no-clobber config write.
//
// Only the host-scope `chips` key is normalized (the one the daemon auto-seeds),
// matching the host-only default-seed above. A project-scoped override
// (`project.<enc>.chips`) carrying legacy tags is left alone — the apps filter
// fill-only chips on decode, so it degrades gracefully until that project's
// chips are re-saved from the editor.
func migrateLegacyChips() {
	raw := readConfigValue(configKeyChips)
	if raw == "" {
		return
	}
	// Parse into maps so we can tell an absent `autosend` from an explicit false
	// (a struct bool can't) and detect the legacy `context` key by presence.
	var entries []map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &entries) != nil {
		return
	}
	needsRewrite := false
	for _, e := range entries {
		if _, ok := e["context"]; ok {
			needsRewrite = true
			break
		}
		if _, ok := e["autosend"]; ok {
			needsRewrite = true
			break
		}
	}
	if !needsRewrite {
		return // already flat & flagless — nothing to migrate
	}
	flat := make([]configChip, 0, len(entries))
	for _, e := range entries {
		if ctx, ok := e["context"]; ok {
			var s string
			if json.Unmarshal(ctx, &s) == nil && s == "tickets" {
				continue // superseded by runtime stage chips
			}
		}
		if auto, ok := e["autosend"]; ok {
			var b bool
			if json.Unmarshal(auto, &b) == nil && !b {
				continue // fill-only template — drop rather than auto-send
			}
		}
		var label, expanded string
		_ = json.Unmarshal(e["label"], &label)
		_ = json.Unmarshal(e["expanded"], &expanded)
		flat = append(flat, configChip{Label: label, Expanded: expanded})
	}
	encoded, err := json.Marshal(flat)
	if err != nil {
		log.Printf("daemon: could not encode migrated chips: %v", err)
		return
	}
	if err := writeConfigValue(configKeyChips, string(encoded)); err != nil {
		log.Printf("daemon: could not persist migrated chips: %v", err)
	}
}
