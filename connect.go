//go:build darwin || linux

package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func runConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	resume := fs.String("resume", "", "Resume a previous session by ID")
	deviceID := fs.String("device-id", "", "Device ID (overrides GREENLIGHT_DEVICE_ID env and config file)")
	project := fs.String("project", "", "Project name (overrides GREENLIGHT_PROJECT env and config file)")
	agentFlag := fs.String("agent", "", "Agent runtime: claude, codex, copilot, cursor, pi (overrides GREENLIGHT_AGENT env and config file)")
	fs.Parse(args)

	if wsURL == "" {
		fmt.Fprintf(os.Stderr, "greenlight: no relay server URL configured (binary must be built with -ldflags)\n")
		os.Exit(1)
	}

	// Resolve agent runtime: flag > env > config > default
	agent := resolveAgent(*agentFlag)
	if !knownAgents[agent] {
		fmt.Fprintf(os.Stderr, "greenlight: unknown agent %q (supported: claude, codex, copilot, cursor, pi)\n", agent)
		os.Exit(1)
	}

	// Build the agent command
	command := agentBinary(agent)
	var cmdArgs []string

	// Generate a session ID for agents that support it. This lets us
	// derive the transcript path deterministically, avoiding the bug where
	// two concurrent sessions in the same CWD pick up the same transcript.
	var agentSessionID string
	if *resume != "" {
		// Resuming: the conversation ID IS the agent session ID (for Claude/Copilot/Pi)
		agentSessionID = *resume
	} else if agent == "claude" || agent == "copilot" || agent == "pi" {
		agentSessionID = generateUUID()
	}

	// Agent-specific resume handling
	if *resume != "" {
		switch agent {
		case "codex":
			// Codex uses a subcommand: codex resume <id>
			cmdArgs = append(cmdArgs, "resume", *resume)
		case "pi":
			// Pi's --resume is a boolean flag (no ID arg), so we can't pass
			// a conversation ID to it. Error out rather than silently ignoring.
			fmt.Fprintf(os.Stderr, "greenlight: --resume is not supported for pi; use /resume to pick a session once pi starts\n")
			os.Exit(1)
		default:
			cmdArgs = append(cmdArgs, "--resume", *resume)
		}
	}
	// Agent-specific flags
	switch agent {
	case "copilot":
		// Bypass built-in prompts so interpose handles permissions
		cmdArgs = append(cmdArgs, "--allow-all")
		// Set session ID for new sessions (--resume also works for new Copilot sessions)
		if *resume == "" && agentSessionID != "" {
			cmdArgs = append(cmdArgs, "--resume", agentSessionID)
		}
	case "cursor":
		// Bypass built-in prompts so interpose handles permissions
		cmdArgs = append(cmdArgs, "--yolo")
	case "codex":
		// Bypass built-in prompts and sandbox so interpose handles permissions
		cmdArgs = append(cmdArgs, "--dangerously-bypass-approvals-and-sandbox")
	case "claude":
		cmdArgs = append(cmdArgs, "--dangerously-skip-permissions", "--append-system-prompt", greenlightSystemPrompt)
		// Set session ID for new sessions so transcript path is deterministic
		if *resume == "" && agentSessionID != "" {
			cmdArgs = append(cmdArgs, "--session-id", agentSessionID)
		}
	case "pi":
		// Pi runs in YOLO mode by default (no bypass flag needed).
		// Inject greenlight instructions via --append-system-prompt.
		cmdArgs = append(cmdArgs, "--append-system-prompt", greenlightSystemPrompt)
		// Use --session <path> for both new and resumed sessions.
		// Pi's --resume is a boolean flag (no ID), so we control the
		// session file directly to get deterministic transcript paths.
		if agentSessionID != "" {
			sessPath := piSessionPath(agentSessionID)
			if sessPath != "" {
				cmdArgs = append(cmdArgs, "--session", sessPath)
			}
		}
	}

	// Resolve device ID: flag > env > config file
	devID := *deviceID
	if devID == "" {
		devID = os.Getenv("GREENLIGHT_DEVICE_ID")
	}
	if devID == "" {
		devID = readConfigValue("device_id")
	}
	if devID == "" {
		fmt.Fprintf(os.Stderr, "greenlight: device ID is required (use --device-id, GREENLIGHT_DEVICE_ID, or set device_id in ~/.greenlight/config)\n")
		fmt.Fprintf(os.Stderr, "greenlight: your device ID can be found on the About tab in the Greenlight app\n")
		os.Exit(1)
	}

	// Resolve project: flag > env > config file (required)
	proj := *project
	if proj == "" {
		proj = os.Getenv("GREENLIGHT_PROJECT")
	}
	if proj == "" {
		proj = readConfigValue("project")
	}
	if proj == "" {
		if wd, err := os.Getwd(); err == nil {
			proj = filepath.Base(wd)
		}
	}
	if proj == "" {
		fmt.Fprintf(os.Stderr, "greenlight: project name is required (use --project)\n")
		os.Exit(1)
	}

	// Reuse relay ID for resumed conversations so the phone sees the same session
	var relayID string
	if *resume != "" {
		relayID = lookupRelayID(*resume)
	}
	if relayID == "" {
		relayID = generateUUID()
	}
	// For Codex, use relay ID as the sentinel for transcript matching.
	// The relay ID is embedded in AGENTS.md and serialized into the transcript.
	if agent == "codex" {
		agentSessionID = relayID
	}
	dialURL := wsURL
	u, err := url.Parse(dialURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: bad relay URL: %v\n", err)
		os.Exit(1)
	}
	q := u.Query()
	q.Set("relay_id", relayID)
	q.Set("project", proj)
	q.Set("agent", agentServerName(agent))
	if version != "" {
		q.Set("version", version)
	}
	u.RawQuery = q.Encode()
	dialURL = u.String()

	// Derive HTTP base URL for enrollment
	baseURL, err := serverBaseURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	// Get CWD for enrollment and session tracking
	cwd, _ := os.Getwd()

	// Enroll session with the relay server
	if err := enrollSession(baseURL, devID, relayID, proj, agentServerName(agent), cwd); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: session enrollment failed: %v\n", err)
		os.Exit(1)
	}

	// Gemini still needs its policy file to auto-approve tools at the CLI level,
	// so interpose handles permissions without Gemini double-prompting.
	if agent == "gemini" {
		if err := installGeminiPolicy(); err != nil {
			log.Printf("Warning: failed to install gemini policy: %v", err)
		}
	}

	// Install agent instruction files for Gemini/Copilot/Cursor (Claude uses --append-system-prompt).
	// These teach the agent how to interpret [GREENLIGHT] permission denial messages.
	// Pi uses --append-system-prompt (like Claude), so no instruction file needed.
	if agent == "gemini" || agent == "copilot" || agent == "cursor" || agent == "codex" {
		if err := installGreenlightInstructions(agent, relayID); err != nil {
			log.Printf("Warning: failed to install agent instructions: %v", err)
		}
	}

	// Write connect PID file for session tracking
	connectPidFile := writeConnectPid(relayID, agent, cwd)
	defer func() {
		os.Remove(connectPidFile)
		cleanupAgentFiles(agent, cwd)
	}()

	// Create bridge file for transcript relay
	bridgePath := filepath.Join(os.TempDir(), "greenlight-bridge-"+relayID)
	if f, err := os.Create(bridgePath); err == nil {
		f.Close()
	}
	defer os.Remove(bridgePath)

	// Export greenlight vars into the child process
	exportEnvs := map[string]string{
		"GREENLIGHT_DEVICE_ID":  devID,
		"GREENLIGHT_SESSION_ID": relayID,
		"GREENLIGHT_PROJECT":    proj,
		"GREENLIGHT_BRIDGE":     bridgePath,
		"GREENLIGHT_AGENT":      agent,
	}

	// Set up library interposition
	libPath, libExtracted := findInterposeLib()
	if libPath != "" {
		if libExtracted {
			defer os.Remove(libPath)
		}
		var logPath string
		if version == "" || version == "dev" {
			logPath = filepath.Join(os.TempDir(), "greenlight-interpose-"+relayID+".log")
			exportEnvs["GREENLIGHT_INTERPOSE_LOG"] = logPath
		}
		if runtime.GOOS == "darwin" {
			// For cursor, check the bundled node binary (not the bash wrapper script)
			entitlementTarget := command
			if agent == "cursor" {
				if nodeBin := resolveCursorNodeBin(command); nodeBin != "" {
					entitlementTarget = nodeBin
				}
			}
			if err := ensureDyldEntitlement(entitlementTarget); err != nil {
				log.Printf("Interposition: skipping, cannot ensure dyld entitlement: %v", err)
			} else {
				exportEnvs["DYLD_INSERT_LIBRARIES"] = libPath
				// Re-sign agent helper binaries so they inherit DYLD_INSERT_LIBRARIES
				ensureAgentHelpers(agent)
			}
		} else {
			exportEnvs["LD_PRELOAD"] = libPath
		}
		// Start permission socket for interpose library
		sockPath, sockCleanup := startInterposeSock(relayID, baseURL, devID, proj, agentServerName(agent))
		if sockPath != "" {
			exportEnvs["GREENLIGHT_INTERPOSE_SOCK"] = sockPath
			defer sockCleanup()
			log.Printf("Interposition socket: %s", sockPath)
		}
		log.Printf("Interposition library: %s", libPath)
		if logPath != "" {
			log.Printf("Interposition log: %s", logPath)
		}
	}

	// If we're injecting a dylib and the command is a script, launch the
	// interpreter directly to avoid /usr/bin/env stripping DYLD_INSERT_LIBRARIES.
	if exportEnvs["DYLD_INSERT_LIBRARIES"] != "" {
		if agent == "cursor" {
			// Cursor's `agent` is a bash script that execs node. We can't
			// resolve to bash (SIP-protected, strips DYLD_INSERT_LIBRARIES).
			// Instead, extract the bundled node binary and index.js directly.
			command, cmdArgs = resolveCursorCommand(command, cmdArgs)
		} else {
			command, cmdArgs = resolveScriptCommand(command, cmdArgs)
		}
	}

	r, err := New(command, cmdArgs, dialURL, devID, WSModeRW, exportEnvs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		os.Exit(1)
	}

	// Enable terminal permission prompts
	promptRelay.mu.Lock()
	promptRelay.r = r
	promptRelay.mu.Unlock()
	defer func() {
		promptRelay.mu.Lock()
		promptRelay.r = nil
		promptRelay.mu.Unlock()
	}()

	// Set kill function for remote "pull the plug"
	if r.ws != nil {
		r.ws.killFunc = func() {
			if r.cmd.Process != nil {
				r.killed = true
				syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}

	// Start bridge tailer — sends transcript lines from bridge file over WebSocket
	var bridgeDone chan struct{}
	var bridgeFinished chan struct{}
	if r.ws != nil {
		bridgeDone = make(chan struct{})
		bridgeFinished = make(chan struct{})
		go func() {
			tailBridge(bridgePath, r.ws, bridgeDone)
			close(bridgeFinished)
		}()
	}

	// Start transcript streamer — polls for the agent's transcript file
	// to appear, then spawns `greenlight stream` to write to the bridge file.
	// Record start time so we only pick up transcripts created after launch.
	// Use a context so polling stops when the session ends.
	startTime := time.Now()
	transcriptCtx, transcriptCancel := context.WithCancel(context.Background())
	go startTranscriptStreamer(transcriptCtx, agent, relayID, agentSessionID, bridgePath, startTime)

	runErr := r.Run()
	transcriptCancel()

	// Kill the detached transcript streamer process.
	killStreamer(relayID)

	// Signal bridge tailer to drain remaining lines and wait for it
	// to finish. This must happen before closing the WebSocket.
	if bridgeDone != nil {
		close(bridgeDone)
		<-bridgeFinished
	}

	r.CloseWS()

	// If the session was killed remotely, print resume instructions
	if r.killed {
		if convID := lookupConversationID(relayID); convID != "" {
			fmt.Fprintf(os.Stderr, "\nTo resume this conversation use --resume %s\n", convID)
		}
	}

	if runErr != nil {
		os.Exit(1)
	}
}

// startTranscriptStreamer polls for the agent's transcript file to appear,
// then spawns `greenlight stream --bridge` to tail it into the bridge file.
func startTranscriptStreamer(ctx context.Context, agent, relayID, agentSessionID, bridgePath string, notBefore time.Time) {
	// Poll until the agent creates its transcript file or the session ends.
	// Some agents (e.g. Codex) only create the file on first user prompt,
	// so we poll for the entire session lifetime rather than using a fixed cap.
	var transcriptPath string
	for {
		select {
		case <-ctx.Done():
			log.Printf("Transcript: session ended before transcript file appeared for %s", agent)
			return
		case <-time.After(500 * time.Millisecond):
		}
		p := deriveTranscriptPath(agent, agentSessionID)
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if agentSessionID != "" || info.ModTime().After(notBefore) {
			transcriptPath = p
			break
		}
	}
	// Re-derive after a short delay to handle the race where an old file gets
	// a brief mtime bump (e.g. Gemini touches previous session during init).
	// If a newer file appeared, switch to it. Skip for deterministic paths.
	if agentSessionID == "" {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if p := deriveTranscriptPath(agent, ""); p != "" && p != transcriptPath {
			log.Printf("Transcript: switching from %s to %s", transcriptPath, p)
			transcriptPath = p
		}
	}
	log.Printf("Transcript: found %s", transcriptPath)

	// Extract conversation ID from transcript filename and persist the
	// conversation→relay mapping so resumed sessions reuse the same relay ID.
	// This works for all agents (previously only saved from SessionStart hook).
	base := filepath.Base(transcriptPath)
	if ext := filepath.Ext(base); ext != "" {
		convID := strings.TrimSuffix(base, ext)
		if convID != "" {
			saveRelayID(convID, relayID)
			log.Printf("Transcript: saved relay mapping %s → %s", convID, relayID)
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("Transcript: failed to resolve executable: %v", err)
		return
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	cmdArgs := []string{"stream",
		"--transcript", transcriptPath,
		"--session-id", relayID,
		"--relay-id", relayID,
		"--bridge", bridgePath,
		"--agent", agent,
	}
	cmd := exec.Command(exePath, cmdArgs...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		log.Printf("Transcript: failed to start streamer: %v", err)
		return
	}

	// Write PID file so future hooks don't spawn a duplicate
	pidFile := filepath.Join(os.TempDir(), "greenlight-stream-"+relayID+".pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %s", cmd.Process.Pid, relayID)), 0644)

	cmd.Process.Release()
}

// killStreamer kills the detached transcript streamer process and removes its PID file.
func killStreamer(relayID string) {
	pidFile := filepath.Join(os.TempDir(), "greenlight-stream-"+relayID+".pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	fields := strings.SplitN(string(data), " ", 2)
	if len(fields) < 1 {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil || pid <= 0 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		proc.Kill()
	}
	os.Remove(pidFile)
	log.Printf("Killed streamer process %d", pid)
}

// writeConnectPid writes a PID file for this connect session.
// Format: <pid> <agent> <cwd>
func writeConnectPid(relayID, agent, cwd string) string {
	p := filepath.Join(os.TempDir(), "greenlight-connect-"+relayID+".pid")
	os.WriteFile(p, []byte(fmt.Sprintf("%d %s %s", os.Getpid(), agent, cwd)), 0644)
	return p
}

// cleanupAgentFiles removes agent-specific files (e.g. gemini policy) if no
// other greenlight connect sessions are active for the same agent and project dir.
func cleanupAgentFiles(agent, cwd string) {
	if hasOtherSessions(agent, cwd) {
		return
	}
	switch agent {
	case "gemini":
		policyPath := filepath.Join(cwd, ".gemini", "policies", "greenlight.toml")
		if err := os.Remove(policyPath); err == nil {
			log.Printf("Removed gemini policy %s", policyPath)
		}
		removeGreenlightInstructions(filepath.Join(cwd, "GEMINI.md"))
	case "copilot":
		removeGreenlightInstructions(filepath.Join(cwd, ".github", "copilot-instructions.md"))
	case "cursor":
		removeGreenlightInstructions(filepath.Join(cwd, ".cursor", "rules", "greenlight.mdc"))
	case "codex":
		removeGreenlightInstructions(filepath.Join(cwd, "AGENTS.md"))
	}
}

// hasOtherSessions checks if any other greenlight connect processes are alive
// for the same agent and working directory.
func hasOtherSessions(agent, cwd string) bool {
	pattern := filepath.Join(os.TempDir(), "greenlight-connect-*.pid")
	matches, _ := filepath.Glob(pattern)
	myPid := os.Getpid()
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		parts := strings.SplitN(string(data), " ", 3)
		if len(parts) < 3 {
			continue
		}
		var pid int
		fmt.Sscanf(parts[0], "%d", &pid)
		if pid == myPid || pid == 0 {
			continue
		}
		pAgent := parts[1]
		pCwd := parts[2]
		if pAgent != agent || pCwd != cwd {
			continue
		}
		// Check if the process is still alive and is a greenlight process
		if isGreenlightProcess(pid) {
			return true
		}
		// Stale PID file — clean it up
		os.Remove(p)
	}
	return false
}

// isGreenlightProcess checks if a PID is alive and belongs to a greenlight process.
func isGreenlightProcess(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(nil) != nil {
		return false
	}
	// Verify it's actually a greenlight process (PIDs can be recycled)
	out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "greenlight")
}

// ensureAgentHelpers re-signs agent helper binaries that need
// DYLD_INSERT_LIBRARIES to be inherited (e.g. Copilot's spawn-helper).
func ensureAgentHelpers(agent string) {
	switch agent {
	case "copilot":
		ensureCopilotHelpers()
	case "codex":
		ensureCodexBinary()
	}
}

func ensureCodexBinary() {
	// The codex npm package is a Node.js wrapper that spawns a native Rust
	// binary from a vendor directory. The Rust binary has hardened runtime
	// (signed by OpenAI) which strips DYLD_INSERT_LIBRARIES. We need to
	// re-sign it with the dyld entitlement.
	codexScript, err := exec.LookPath("codex")
	if err != nil {
		return
	}
	resolved, err := filepath.EvalSymlinks(codexScript)
	if err != nil {
		resolved = codexScript
	}
	// The vendor binary is at: <npm-prefix>/lib/node_modules/@openai/codex/
	//   node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/codex/codex
	// Navigate from the script to the package root
	pkgRoot := filepath.Dir(filepath.Dir(resolved))
	// Determine platform package name
	var platformPkg, triple string
	switch runtime.GOARCH {
	case "arm64":
		platformPkg = "@openai/codex-darwin-arm64"
		triple = "aarch64-apple-darwin"
	case "amd64":
		platformPkg = "@openai/codex-darwin-x64"
		triple = "x86_64-apple-darwin"
	default:
		return
	}
	binaryPath := filepath.Join(pkgRoot, "node_modules", platformPkg, "vendor", triple, "codex", "codex")
	if _, err := os.Stat(binaryPath); err != nil {
		log.Printf("Interposition: codex binary not found at %s", binaryPath)
		return
	}
	if err := ensureDyldEntitlement(binaryPath); err != nil {
		log.Printf("Interposition: codex binary: %v", err)
	}
}

func ensureCopilotHelpers() {
	// Copilot's spawn-helper is used for bash tool invocations.
	// Without the dyld entitlement, macOS strips DYLD_INSERT_LIBRARIES
	// from spawn-helper, so inner commands (find, cat, etc.) are uninterposed.
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	spawnHelper := filepath.Join(home, ".copilot", "pkg", "darwin-arm64", "1.0.2", "prebuilds", "darwin-arm64", "spawn-helper")
	if _, err := os.Stat(spawnHelper); err != nil {
		return
	}
	if err := ensureDyldEntitlement(spawnHelper); err != nil {
		log.Printf("Interposition: spawn-helper: %v", err)
	}
}

// ensureDyldEntitlement checks if the agent binary has the
// com.apple.security.cs.allow-dyld-environment-variables entitlement.
// If not, it re-signs the binary to add it (ad-hoc, no developer identity needed).
func ensureDyldEntitlement(command string) error {
	binPath, err := exec.LookPath(command)
	if err != nil {
		return fmt.Errorf("cannot find binary %q: %w", command, err)
	}

	// If the resolved path is a script (not a Mach-O binary), check the
	// interpreter instead. Scripts inherit DYLD_INSERT_LIBRARIES from
	// their interpreter (e.g. node), so we need the interpreter to have
	// the entitlement, not the script itself.
	binPath, err = resolveInterpreter(binPath)
	if err != nil {
		return fmt.Errorf("resolve interpreter for %q: %w", command, err)
	}

	// Check current entitlements
	out, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", binPath).Output()
	if err != nil {
		// Not signed at all — signing will add the entitlement
		log.Printf("Interposition: binary not signed, will sign: %s", binPath)
	} else if strings.Contains(string(out), "allow-dyld-environment-variables") {
		return nil // already has it
	}

	log.Printf("Interposition: re-signing %s to add dyld entitlement", binPath)

	// Build entitlements plist preserving existing ones + adding dyld
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.cs.allow-jit</key>
    <true/>
    <key>com.apple.security.cs.allow-unsigned-executable-memory</key>
    <true/>
    <key>com.apple.security.cs.disable-library-validation</key>
    <true/>
    <key>com.apple.security.cs.allow-dyld-environment-variables</key>
    <true/>
</dict>
</plist>`

	plistPath := filepath.Join(os.TempDir(), "greenlight-entitlements.plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("write entitlements plist: %w", err)
	}
	defer os.Remove(plistPath)

	// Two-step re-sign: remove old signature, then sign fresh.
	// Using --force corrupts Node.js SEA binaries; remove+sign preserves them.
	rmCmd := exec.Command("codesign", "--remove-signature", binPath)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codesign remove: %s: %w", string(out), err)
	}

	signCmd := exec.Command("codesign", "--sign", "-",
		"--entitlements", plistPath,
		"--options", "runtime",
		binPath)
	if out, err := signCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codesign sign: %s: %w", string(out), err)
	}

	log.Printf("Interposition: re-signed %s successfully", binPath)
	return nil
}

// resolveScriptCommand checks if command is a script. If so, it rewrites the
// command and args to invoke the interpreter directly (e.g. "node /path/to/gemini ...").
// This avoids /usr/bin/env (which lacks the dyld entitlement) stripping
// DYLD_INSERT_LIBRARIES from the environment.
func resolveScriptCommand(command string, args []string) (string, []string) {
	binPath, err := exec.LookPath(command)
	if err != nil {
		return command, args
	}

	f, err := os.Open(binPath)
	if err != nil {
		return command, args
	}
	defer f.Close()

	header := make([]byte, 2)
	if _, err := f.Read(header); err != nil || string(header) != "#!" {
		return command, args // not a script
	}

	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)

	// Parse shebang: extract interpreter and its flags
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return command, args
	}

	var interpArgs []string
	interp := parts[0]
	if filepath.Base(interp) == "env" {
		// Skip "env" and its flags (like -S), find the actual interpreter
		for i, p := range parts[1:] {
			if !strings.HasPrefix(p, "-") {
				interp = p
				interpArgs = parts[i+2:] // remaining flags after interpreter name
				break
			}
		}
	} else {
		interpArgs = parts[1:]
	}

	// Resolve interpreter to absolute path
	resolved, err := exec.LookPath(interp)
	if err != nil {
		return command, args
	}

	// Build new args: [interpreter flags...] [script path] [original args...]
	newArgs := make([]string, 0, len(interpArgs)+1+len(args))
	newArgs = append(newArgs, interpArgs...)
	newArgs = append(newArgs, binPath)
	newArgs = append(newArgs, args...)

	log.Printf("Interposition: launching %s %s (bypassing shebang)", resolved, strings.Join(newArgs, " "))
	return resolved, newArgs
}

// resolveInterpreter checks if binPath is a script with a shebang line.
// If so, it resolves and returns the interpreter binary path.
// If it's a binary (or shebang can't be read), returns binPath unchanged.
func resolveInterpreter(binPath string) (string, error) {
	f, err := os.Open(binPath)
	if err != nil {
		return binPath, nil
	}
	defer f.Close()

	// Read the first two bytes to check for shebang
	header := make([]byte, 2)
	if _, err := f.Read(header); err != nil || string(header) != "#!" {
		return binPath, nil // not a script
	}

	// Read the shebang line
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)

	// Handle "/usr/bin/env [-S] interpreter [args...]"
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return binPath, nil
	}
	interp := parts[0]
	if filepath.Base(interp) == "env" && len(parts) > 1 {
		// Skip env flags like -S
		for _, p := range parts[1:] {
			if !strings.HasPrefix(p, "-") {
				interp = p
				break
			}
		}
	}

	// Resolve the interpreter to an absolute path
	resolved, err := exec.LookPath(interp)
	if err != nil {
		return binPath, nil // can't resolve, fall back to original
	}
	log.Printf("Interposition: %s is a script, checking interpreter %s", binPath, resolved)
	return resolved, nil
}

// findInterposeLib extracts the embedded interposition library.
// Returns the path and whether the caller should remove it on exit.
func findInterposeLib() (string, bool) {
	if p := extractEmbeddedLib(); p != "" {
		return p, true
	}
	return "", false
}

// resolveCursorNodeBin finds the bundled node binary from the cursor agent script.
func resolveCursorNodeBin(command string) string {
	binPath, err := exec.LookPath(command)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		return ""
	}
	nodeBin := filepath.Join(filepath.Dir(resolved), "node")
	if _, err := os.Stat(nodeBin); err != nil {
		return ""
	}
	return nodeBin
}

// resolveCursorCommand resolves the Cursor `agent` shell script to its bundled
// node binary and index.js. The agent script structure is:
//
//	SCRIPT_DIR="$(dirname "$(realpath "$0")")"
//	exec "$SCRIPT_DIR/node" --use-system-ca "$SCRIPT_DIR/index.js" "$@"
//
// We replicate this to launch node directly, so DYLD_INSERT_LIBRARIES is
// inherited (bash is SIP-protected and strips it).
func resolveCursorCommand(command string, args []string) (string, []string) {
	binPath, err := exec.LookPath(command)
	if err != nil {
		return command, args
	}
	resolved, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		resolved = binPath
	}
	scriptDir := filepath.Dir(resolved)

	nodeBin := filepath.Join(scriptDir, "node")
	indexJS := filepath.Join(scriptDir, "index.js")

	// Verify both files exist
	if _, err := os.Stat(nodeBin); err != nil {
		log.Printf("Interposition: cursor node not found at %s, falling back", nodeBin)
		return resolveScriptCommand(command, args)
	}
	if _, err := os.Stat(indexJS); err != nil {
		log.Printf("Interposition: cursor index.js not found at %s, falling back", indexJS)
		return resolveScriptCommand(command, args)
	}

	newArgs := []string{"--use-system-ca", indexJS}
	newArgs = append(newArgs, args...)

	// Set CURSOR_INVOKED_AS — the bash wrapper normally does this
	os.Setenv("CURSOR_INVOKED_AS", filepath.Base(binPath))

	log.Printf("Interposition: launching %s %s (bypassing cursor bash wrapper)", nodeBin, strings.Join(newArgs, " "))
	return nodeBin, newArgs
}

// detachedSysProcAttr returns SysProcAttr for a detached subprocess.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}

func generateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
