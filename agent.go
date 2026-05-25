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
func resolveAgent(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("GREENLIGHT_AGENT"); v != "" {
		return v
	}
	if v := readConfigValue("agent"); v != "" {
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

// greenlightSystemPrompt is appended to the agent's system prompt to teach it
// how to interpret permission denials from the Greenlight interpose library.
// Bare "greenlight" works across prod/dev/local builds because every session
// prepends a per-session bin dir to the agent's PATH with a symlink that
// points at the running binary (see setupCLIShim).
const greenlightSystemPrompt = `Tool calls are managed by a permission system called Greenlight. ` +
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
	`OAuth access tokens are stored as ${PROVIDER}_ACCESS_TOKEN (e.g. GITHUB_ACCESS_TOKEN) and refresh automatically when expired.`

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

	if err := writeConfigValue("agent", name); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Default agent set to %s\n", name)
}
