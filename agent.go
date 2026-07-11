//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// knownAgents lists the valid agent runtime values.
var knownAgents = map[string]bool{
	"claude":  true,
	"copilot": true,
	"cursor":  true,
	"codex":   true,
	"gemini":  true,
	"pi":      true,
}

const defaultAgent = "claude"

// resolveAgent resolves the agent runtime from flag > env > config > default.
// The config lookup is host-scoped (no project context at this call site).
func resolveAgent(flagVal string) string {
	return resolveAgentForProject(flagVal, "")
}

// resolveAgentForProject is resolveAgent with project-override awareness: a
// project's `agent` config entry shadows the host default. flag and env still
// win over config.
func resolveAgentForProject(flagVal, project string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("GREENLIGHT_AGENT"); v != "" {
		return v
	}
	if v := resolveConfig(project, configKeyAgent); v != "" {
		return v
	}
	return defaultAgent
}

// agentBinary returns the CLI binary name for the given agent runtime.
func agentBinary(agent string) string {
	switch agent {
	case "gemini":
		return "gemini"
	case "copilot":
		return "copilot"
	case "cursor":
		return "agent"
	case "codex":
		return "codex"
	case "pi":
		return "pi"
	default:
		return "claude"
	}
}

// agentServerName returns the agent identifier sent to the server.
func agentServerName(agent string) string {
	switch agent {
	case "gemini":
		return "gemini"
	case "copilot":
		return "copilot"
	case "cursor":
		return "cursor"
	case "codex":
		return "codex"
	case "pi":
		return "pi"
	default:
		return "claude-code"
	}
}

// skillsRoot returns the path under cwd where the given agent discovers skills,
// per each agent's documented search paths. All six supported agents conform to
// the open SKILL.md standard (agentskills.io); only the discovery root differs.
// Returns "" if skills aren't supported for the agent.
func skillsRoot(agent string) string {
	switch agent {
	case "claude":
		return filepath.Join(".claude", "skills")
	case "codex":
		return filepath.Join(".agents", "skills")
	case "gemini":
		return filepath.Join(".gemini", "skills")
	case "cursor":
		return filepath.Join(".cursor", "skills")
	case "copilot":
		return filepath.Join(".github", "skills")
	case "pi":
		return filepath.Join(".pi", "skills")
	default:
		return ""
	}
}

// agentSupportsResume returns whether the agent supports --resume with a session ID.
func agentSupportsResume(agent string) bool {
	switch agent {
	case "claude", "copilot", "codex", "cursor", "gemini":
		return true
	default:
		return false
	}
}

// greenlightSystemPromptBase is the static portion of the system-prompt
// injection — permission semantics and secret access. ticket-specific
// context, if any, is appended by greenlightSystemPrompt.
// Bare "greenlight" works across prod/dev/local builds because every session
// prepends a per-session bin dir to the agent's PATH with a symlink that
// points at the running binary (see setupCLIShim).
const greenlightSystemPromptBase = `Tool calls are managed by a permission system called Greenlight. ` +
	`If a command exits with code 126, or a file operation fails with "Permission denied", ` +
	`the user has explicitly denied this action. ` +
	`Do not retry the same action. Try a different approach or ask the user what they'd like instead. ` +
	`If a command exits with code 127, or a file operation fails with "Operation not permitted", ` +
	`the user wants you to stop. Do not continue with any further tool calls. ` +
	`Explain what you were doing and wait for new instructions.` +
	"\n\n" +
	`Encrypted secrets (API tokens, OAuth credentials, etc.) may be available via the greenlight CLI. ` +
	`Run "greenlight secrets list" to see available keys. ` +
	`To use one, run "greenlight run -e KEY_NAME -- <command>" — KEY_NAME will be injected as an environment variable for that command only, and the secret value is scrubbed from stdout/stderr before you see it. ` +
	`Prefer this over asking the user to paste tokens. ` +
	`OAuth access tokens are stored as ${PROVIDER}_ACCESS_TOKEN (e.g. GITHUB_ACCESS_TOKEN) and refresh automatically when expired.` +
	"\n\n" +
	`To reduce approval prompts, phrase Bash commands so they match existing auto-approve rules: ` +
	`(1) Run unrelated steps as separate Bash calls — compound commands (&&, ||, |) auto-approve only when every segment matches a rule; one unusual segment blocks the whole chain. ` +
	`(2) Run commands directly rather than wrapping them in "bash -c" or "sh -c" — the server unwraps inline scripts anyway and they won't match wildcard rules. ` +
	`(3) Avoid heredocs: use the Write tool to write files, and pass multi-line git commit messages as repeated -m flags (e.g. git commit -m "summary" -m "details") rather than a heredoc. ` +
	`(4) Some flags are never covered by wildcard rules and always need their own approval: sed -i / --in-place (use the Edit tool instead), curl -X/-d/-F/--data* (non-GET requests), find -exec/-execdir/-delete. ` +
	`(5) rm, mv, chmod, kill, ssh, scp, rsync, and similar destructive or network commands always require exact-match approval — keep them minimal and self-explanatory so they're easy to approve. ` +
	`(6) To read or filter JSON, use "jq" — it is auto-approved. Avoid parsing JSON with interpreter one-liners (python3 -c, node -e, perl -e, ruby -e): they run arbitrary code, can't be auto-approved, and will prompt every time. ` +
	`(7) For text substitutions, prefer "sed" or "awk" (read-only, auto-approved) over perl/python one-liners, which run arbitrary code and always prompt; to change a file in place, use the Edit tool rather than perl -i / sed -i.`

// sshNoIdentityLine is appended to the system prompt when ssh_isolation is on,
// no ssh_keys are configured at all, and the session serves no keys (#249):
// the inherited SSH_AUTH_SOCK/SSH_AGENT_PID were stripped from the child env
// and nothing serves keys, so the agent should report rather than flail when
// ssh fails.
const sshNoIdentityLine = "This session has no SSH identity; ssh to remote hosts will fail — tell the user if you need one."

// sshSkippedKeysLine is appended instead of sshNoIdentityLine when ssh_keys
// named at least one entry but none of them resolved to a live agent key
// (issue #292): the generic "no identity" line reads identically whether the
// user never configured a key or configured one that silently failed to
// load, so the agent (and, via the request log, the user) had no way to tell
// the two apart short of the daemon log. Naming the configured-but-unusable
// secrets here makes the failure mode explicit instead of "ssh just fails".
func sshSkippedKeysLine(skipped []string) string {
	return "This session has no SSH identity even though ssh_keys names " +
		strings.Join(skipped, ", ") +
		" — none of them resolved (missing stored secret or missing/unparseable " +
		"local ~/.greenlight/ssh/<name>.pub). ssh to remote hosts will fail; tell " +
		"the user their configured SSH key(s) did not load rather than assuming " +
		"none were ever set up."
}

// sshManagedAgentLine is the system-prompt line for an isolated session whose
// ssh-agent serves keys (#250): names the short key names so the agent runs
// ssh/git/scp normally instead of hunting for key files. git already forces
// this agent's identity via GIT_SSH_COMMAND regardless of the user's own
// ~/.ssh/config (issue #292 — an IdentitiesOnly/IdentityFile entry for the
// remote host would otherwise make OpenSSH never even offer our agent's key);
// bare ssh isn't wrapped, so the agent is told the explicit override flags.
func sshManagedAgentLine(names []string) string {
	return "SSH is pre-configured via a managed ssh-agent (keys: " +
		strings.Join(names, ", ") +
		"). Run ssh/git/scp normally; do not look for key files or configure " +
		"identities. git already forces this agent's key via GIT_SSH_COMMAND, " +
		"overriding any local ~/.ssh/config identity settings for the remote host. " +
		"For bare ssh (not through git), add the same override explicitly: " +
		`ssh -o IdentityAgent="$SSH_AUTH_SOCK" -o IdentitiesOnly=no <host>.`
}

// greenlightSystemPrompt returns the system-prompt injection for this
// session. When command shims are active at session start, a line names the
// pre-authenticated commands so the agent runs them bare instead of
// hand-rolling `greenlight run` with manual token plumbing (issue #198).
// When ssh_isolation resolved on at session start, a line either names the
// keys the managed ssh-agent serves (#250) or warns that the session has no
// SSH identity (#249). When the session was launched against a specific
// ticket, a single neutral line is appended pointing the agent at the URL —
// what to do with it (read / update / close / stage moves) is left to the
// user's prompt (the app's stage-aware launch chips drive this).
//
// shims is the same []resolvedShim the PATH shim is installed from, and
// sshState the same session-start resolution the env strip and session
// ssh-agent are built from, so the prompt and the actual session env can't
// diverge: shims empty and isolation off leave the prompt byte-for-byte
// unchanged, never claiming a CLI is pre-authenticated or an SSH identity
// missing when the session behaves as today.
func greenlightSystemPrompt(ticket *TicketRef, shims []resolvedShim, sshState sshSession) string {
	prompt := greenlightSystemPromptBase
	if line := shimPreauthLine(shims); line != "" {
		prompt += "\n\n" + line
	}
	if sshState.serving() {
		prompt += "\n\n" + sshManagedAgentLine(sshState.keyNames())
	} else if sshState.isolated && len(sshState.skipped) > 0 {
		prompt += "\n\n" + sshSkippedKeysLine(sshState.skipped)
	} else if sshState.isolated {
		prompt += "\n\n" + sshNoIdentityLine
	}
	if ticket != nil && ticket.URL != "" {
		prompt += "\n\nA ticket is in scope for this session: " + ticket.URL
	}
	return prompt
}

// shimPreauthLine returns the system-prompt sentence naming the commands whose
// greenlight shim is active for this session, or "" when no shim is active.
// The agent should run these bare; greenlight injects the token and scrubs it
// from output, so wrapping them in `greenlight run` or plumbing the env var
// manually is strictly worse (issue #198).
func shimPreauthLine(shims []resolvedShim) string {
	if len(shims) == 0 {
		return ""
	}
	names := make([]string, 0, len(shims))
	for _, s := range shims {
		names = append(names, s.cmd)
	}
	joined := strings.Join(names, ", ")
	return "The following commands are pre-authenticated — run them directly " +
		"(e.g. `" + names[0] + " issue list`); do not wrap them in \"greenlight run\" " +
		"or pass a token manually: " + joined + "."
}

// deriveTranscriptPath constructs the transcript file path for the given agent.
// For Claude it finds the newest .jsonl in the project dir; for Copilot it
// finds the newest session dir. Returns "" if it can't be determined.
func deriveTranscriptPath(agent, sessionID, cwd string) string {
	switch agent {
	case "claude", "claude-code":
		if sessionID != "" {
			return deriveClaudeTranscriptPathByID(sessionID, cwd)
		}
		return deriveClaudeTranscriptPath(cwd)
	case "copilot":
		if sessionID != "" {
			return deriveCopilotTranscriptPathByID(sessionID)
		}
		return deriveCopilotTranscriptPath()
	case "gemini":
		if sessionID != "" {
			return deriveGeminiTranscriptPathByID(sessionID, cwd)
		}
		return deriveGeminiTranscriptPath(cwd)
	case "cursor":
		if sessionID != "" {
			return deriveCursorTranscriptPathByID(sessionID, cwd)
		}
		return deriveCursorTranscriptPath(cwd)
	case "codex":
		// Resume: sessionID is the codex UUID — match by filename.
		if sessionID != "" {
			return deriveCodexTranscriptByUUID(sessionID)
		}
		// Fresh session: codex 0.128+ no longer embeds AGENTS.md content
		// (and so the greenlight sentinel never appears in the rollout),
		// so match the rollout's session_meta.cwd against the agent cwd.
		return deriveCodexTranscriptByCwd(cwd)
	case "pi":
		if sessionID != "" {
			return piSessionPath(sessionID, cwd)
		}
		return derivePiTranscriptPath(cwd)
	default:
		return ""
	}
}

// claudeProjectsDir returns ~/.claude/projects, or "" if the home dir can't be resolved.
func claudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// pathsEqual compares two filesystem paths for equality, resolving symlinks
// when possible so e.g. "/tmp/foo" matches "/private/tmp/foo" on macOS.
// Falls back to literal equality if a path can't be resolved (e.g. doesn't exist).
func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return ra == rb
	}
	return false
}

// jsonlCwdMatches reports whether the JSONL transcript at path contains an
// entry whose "cwd" field matches cwd. Claude Code's project-directory naming
// scheme isn't fully documented and may transform special characters, so we
// identify the right transcript by reading the file rather than guessing the
// encoded directory name.
func jsonlCwdMatches(path, cwd string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	// cwd typically appears within the first handful of lines (after the
	// summary/snapshot header). Read a bounded prefix to keep this cheap.
	for i := 0; i < 20 && scanner.Scan(); i++ {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Cwd != "" && pathsEqual(rec.Cwd, cwd) {
			return true
		}
	}
	return false
}

// deriveClaudeTranscriptPathByID returns the transcript path for a known session ID.
// The file may not exist yet (the caller polls until it appears).
func deriveClaudeTranscriptPathByID(sessionID, cwd string) string {
	root := claudeProjectsDir()
	if root == "" {
		return ""
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name(), sessionID+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// deriveClaudeTranscriptPath finds the newest .jsonl across all Claude project
// directories whose contents indicate it belongs to the given cwd.
func deriveClaudeTranscriptPath(cwd string) string {
	root := claudeProjectsDir()
	if root == "" {
		return ""
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		// Find the newest .jsonl in this project dir first, then validate cwd.
		// Avoids reading every transcript on every poll.
		var candidate string
		var candidateTime time.Time
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(candidateTime) {
				candidateTime = info.ModTime()
				candidate = e.Name()
			}
		}
		if candidate == "" || !candidateTime.After(newestTime) {
			continue
		}
		path := filepath.Join(root, d.Name(), candidate)
		if !jsonlCwdMatches(path, cwd) {
			continue
		}
		newest = path
		newestTime = candidateTime
	}
	return newest
}

// deriveCopilotTranscriptPathByID returns the transcript path for a known session ID.
func deriveCopilotTranscriptPathByID(sessionID string) string {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".copilot")
	}
	return filepath.Join(home, "session-state", sessionID, "events.jsonl")
}

func deriveCopilotTranscriptPath() string {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".copilot")
	}
	stateDir := filepath.Join(home, "session-state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(stateDir, newest, "events.jsonl")
	}
	return ""
}

// deriveGeminiTranscriptPathByID finds the transcript file whose sessionId
// JSON field matches the given UUID.
// findGeminiProjectDir scans ~/.gemini/tmp/* for the project dir whose
// .project_root file contains cwd. Gemini encodes the cwd basename when
// naming the dir, so we can't reliably reconstruct the name; reading
// .project_root is authoritative.
func findGeminiProjectDir(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	root := filepath.Join(home, ".gemini", "tmp")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name())
		data, err := os.ReadFile(filepath.Join(p, ".project_root"))
		if err != nil {
			continue
		}
		if pathsEqual(strings.TrimSpace(string(data)), cwd) {
			return p
		}
	}
	return ""
}

func deriveGeminiTranscriptPathByID(sessionID, cwd string) string {
	projDir := findGeminiProjectDir(cwd)
	if projDir == "" {
		return ""
	}
	chatsDir := filepath.Join(projDir, "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !isGeminiTranscriptFile(e.Name()) {
			continue
		}
		p := filepath.Join(chatsDir, e.Name())
		if extractGeminiSessionID(p) == sessionID {
			return p
		}
	}
	return ""
}

func deriveGeminiTranscriptPath(cwd string) string {
	projDir := findGeminiProjectDir(cwd)
	if projDir == "" {
		return ""
	}
	chatsDir := filepath.Join(projDir, "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !isGeminiTranscriptFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(chatsDir, newest)
	}
	return ""
}

// isGeminiTranscriptFile returns true if name looks like a Gemini transcript.
// Older Gemini (≤0.38) writes session-*.json; newer (≥0.40) writes session-*.jsonl.
func isGeminiTranscriptFile(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl")
}

// readGeminiTranscript parses a Gemini transcript file in either supported layout
// and returns the sessionId and the message records. The .jsonl format (Gemini
// ≥0.40) has a metadata line followed by per-message lines and `$set` patch ops
// (which are filtered out). The .json format is a single object with sessionId
// and messages array.
func readGeminiTranscript(path string) (string, []json.RawMessage, error) {
	if strings.HasSuffix(path, ".jsonl") {
		f, err := os.Open(path)
		if err != nil {
			return "", nil, err
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 1024*1024), 4*1024*1024)
		var sessionID string
		var messages []json.RawMessage
		first := true
		for s.Scan() {
			line := append([]byte(nil), s.Bytes()...)
			if first {
				first = false
				var meta struct {
					SessionID string `json:"sessionId"`
				}
				if json.Unmarshal(line, &meta) == nil && meta.SessionID != "" {
					sessionID = meta.SessionID
					continue
				}
				// Not the metadata line — fall through and treat as a message candidate.
			}
			var probe map[string]json.RawMessage
			if json.Unmarshal(line, &probe) != nil {
				continue
			}
			if _, set := probe["$set"]; set {
				continue
			}
			if _, hasID := probe["id"]; !hasID {
				continue
			}
			messages = append(messages, json.RawMessage(line))
		}
		return sessionID, messages, s.Err()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var obj struct {
		SessionID string            `json:"sessionId"`
		Messages  []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return "", nil, err
	}
	return obj.SessionID, obj.Messages, nil
}

// extractGeminiSessionID reads the sessionId field from a Gemini transcript file
// (either .json or .jsonl format).
func extractGeminiSessionID(path string) string {
	id, _, _ := readGeminiTranscript(path)
	return id
}

// cursorProjectsDir returns ~/.cursor/projects, or "" if home is unavailable.
func cursorProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cursor", "projects")
}

// cursorProjectMatches reports whether the cursor project dir at projDir
// belongs to cwd. Cursor's worker.log contains a "workspacePath=<cwd>" line
// (unencoded), which we use as a more reliable signal than reverse-engineering
// the directory-name encoding.
func cursorProjectMatches(projDir, cwd string) bool {
	f, err := os.Open(filepath.Join(projDir, "worker.log"))
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for i := 0; i < 50 && scanner.Scan(); i++ {
		line := scanner.Text()
		idx := strings.Index(line, "workspacePath=")
		if idx < 0 {
			continue
		}
		path := strings.TrimSpace(line[idx+len("workspacePath="):])
		// Trim trailing context after the path (e.g. another " key=value" pair).
		if sp := strings.IndexAny(path, " \t"); sp >= 0 {
			path = path[:sp]
		}
		if pathsEqual(path, cwd) {
			return true
		}
	}
	return false
}

// findCursorProjectDir scans ~/.cursor/projects/* for the project dir matching cwd.
// Returns "" if none match (caller polls until cursor writes worker.log).
func findCursorProjectDir(cwd string) string {
	root := cursorProjectsDir()
	if root == "" {
		return ""
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name())
		if cursorProjectMatches(p, cwd) {
			return p
		}
	}
	return ""
}

// deriveCursorTranscriptPathByID returns the transcript path for a known session UUID.
// Cursor uses two layouts: <uuid>.jsonl (old) or <uuid>/<uuid>.jsonl (new).
func deriveCursorTranscriptPathByID(sessionID, cwd string) string {
	projDir := findCursorProjectDir(cwd)
	if projDir == "" {
		return ""
	}
	transcriptsDir := filepath.Join(projDir, "agent-transcripts")
	// New layout: <uuid>/<uuid>.jsonl
	nested := filepath.Join(transcriptsDir, sessionID, sessionID+".jsonl")
	if _, err := os.Stat(nested); err == nil {
		return nested
	}
	// Old layout: <uuid>.jsonl
	flat := filepath.Join(transcriptsDir, sessionID+".jsonl")
	if _, err := os.Stat(flat); err == nil {
		return flat
	}
	return ""
}

func deriveCursorTranscriptPath(cwd string) string {
	projDir := findCursorProjectDir(cwd)
	if projDir == "" {
		return ""
	}
	transcriptsDir := filepath.Join(projDir, "agent-transcripts")
	entries, err := os.ReadDir(transcriptsDir)
	if err != nil {
		return ""
	}
	var newestPath string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() {
			// New layout: <uuid>/<uuid>.jsonl
			nested := filepath.Join(transcriptsDir, e.Name(), e.Name()+".jsonl")
			if info, err := os.Stat(nested); err == nil && info.ModTime().After(newestTime) {
				newestTime = info.ModTime()
				newestPath = nested
			}
		} else if strings.HasSuffix(e.Name(), ".jsonl") {
			// Old layout: <uuid>.jsonl
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(newestTime) {
				newestTime = info.ModTime()
				newestPath = filepath.Join(transcriptsDir, e.Name())
			}
		}
	}
	return newestPath
}

// deriveCodexTranscriptByUUID finds a Codex transcript file whose filename
// contains the given UUID. Codex filenames follow the pattern:
// rollout-YYYY-MM-DDTHH-MM-SS-UUID.jsonl
func deriveCodexTranscriptByUUID(uuid string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	sessionsDir := filepath.Join(home, ".codex", "sessions")
	var match string
	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		if strings.Contains(info.Name(), uuid) {
			match = path
			return filepath.SkipAll
		}
		return nil
	})
	return match
}

// deriveCodexTranscriptByCwd finds the newest codex rollout whose
// session_meta entry records the given working directory. Codex 0.128
// writes session_meta as the first line of the rollout JSONL with a
// payload.cwd field. We scan recent rollouts in mtime order and return
// the first one whose cwd matches. Returns "" if none match — callers
// must NOT fall back to "newest globally," since that latches onto a
// previous session in a sibling cwd.
func deriveCodexTranscriptByCwd(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil || cwd == "" {
		return ""
	}
	target := strings.TrimRight(cwd, "/")
	sessionsDir := filepath.Join(home, ".codex", "sessions")

	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".jsonl") {
			return nil
		}
		candidates = append(candidates, candidate{path, info.ModTime()})
		return nil
	})
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].mtime.After(candidates[j].mtime)
	})

	limit := 20
	if len(candidates) < limit {
		limit = len(candidates)
	}
	for _, c := range candidates[:limit] {
		if matchesCodexCwd(c.path, target) {
			return c.path
		}
	}
	return ""
}

// matchesCodexCwd parses the first line of a codex rollout JSONL as a
// session_meta event and returns true if its payload.cwd matches target.
// session_meta with full base_instructions can exceed bufio's default
// scan buffer, so we use a generous one.
func matchesCodexCwd(path, target string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	if !scanner.Scan() {
		return false
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil {
		return false
	}
	return strings.TrimRight(meta.Payload.Cwd, "/") == target
}

// extractCodexSessionID extracts the UUID from a codex transcript filename.
// Codex filenames follow the pattern: rollout-YYYY-MM-DDTHH-MM-SS-UUID.jsonl
// codex resume expects just the UUID (e.g. "019d812e-9739-7b42-a987-f4029a795306").
func extractCodexSessionID(transcriptPath string) string {
	base := filepath.Base(transcriptPath)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	// UUID is the last 36 characters (8-4-4-4-12 hex format).
	if len(base) >= 36 {
		candidate := base[len(base)-36:]
		// Quick sanity check: dashes at positions 8, 13, 18, 23.
		if len(candidate) == 36 && candidate[8] == '-' && candidate[13] == '-' && candidate[18] == '-' && candidate[23] == '-' {
			return candidate
		}
	}
	// Fallback: return the full filename without extension.
	return base
}

// piSessionPath returns the transcript file path for a Pi session ID,
// following Pi's convention: $PI_CODING_AGENT_DIR/sessions/<normalized-cwd>/<sessionID>.jsonl
// PI_CODING_AGENT_DIR defaults to ~/.pi/agent.
func piSessionPath(sessionID, cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	baseDir := os.Getenv("PI_CODING_AGENT_DIR")
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		baseDir = filepath.Join(home, ".pi", "agent")
	}
	// Pi encodes CWD as: strip leading /, replace /\: with -, wrap in --
	safePath := strings.TrimLeft(cwd, "/\\")
	safePath = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safePath)
	safePath = "--" + safePath + "--"
	return filepath.Join(baseDir, "sessions", safePath, sessionID+".jsonl")
}

func derivePiTranscriptPath(cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Pi encodes CWD as: strip leading /, replace /\: with -, wrap in --
	safePath := strings.TrimLeft(cwd, "/\\")
	safePath = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safePath)
	safePath = "--" + safePath + "--"
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", safePath)
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(sessDir, newest)
	}
	return ""
}

func runAgent(args []string) {
	if len(args) == 0 {
		// Print current agent
		agent := resolveAgent("")
		fmt.Fprintf(os.Stderr, "%s\n", agent)
		return
	}

	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(os.Stderr, "Usage: greenlight agent [name]\n\n")
		fmt.Fprintf(os.Stderr, "Without arguments, prints the current default agent.\n")
		fmt.Fprintf(os.Stderr, "With a name, sets the default agent in ~/.greenlight/config.\n\n")
		fmt.Fprintf(os.Stderr, "Supported agents: claude, codex, copilot, cursor, gemini, pi\n")
		os.Exit(0)
	}

	name := args[0]
	if !knownAgents[name] {
		fmt.Fprintf(os.Stderr, "greenlight: unknown agent %q (supported: claude, codex, copilot, cursor, gemini, pi)\n", name)
		os.Exit(1)
	}
	// Delegate the write to the generic config plumbing so the agent subcommand
	// and `greenlight config set agent` share one path.
	if err := applyConfigBatch(scopeHost, "", map[string]string{configKeyAgent: name}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Default agent set to %s\n", name)
}
