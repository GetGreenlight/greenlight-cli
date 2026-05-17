//go:build darwin || linux

package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
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
	agentFlag := fs.String("agent", "", "Agent runtime: claude, codex, copilot, cursor, gemini, pi (overrides GREENLIGHT_AGENT env and config file)")
	fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "greenlight connect: unexpected argument %q\nRun 'greenlight connect --help' for usage.\n", fs.Arg(0))
		os.Exit(1)
	}

	if wsURL == "" {
		fmt.Fprintf(os.Stderr, "greenlight: no relay server URL configured (binary must be built with -ldflags)\n")
		os.Exit(1)
	}

	// When resuming, load the saved session record and use its values
	// as defaults. Explicit flags still override.
	if *resume != "" {
		if rec, err := loadSessionRecord(*resume); err == nil {
			if *agentFlag == "" {
				*agentFlag = rec.Agent
			}
			if *project == "" {
				*project = rec.Project
			}
			// cd to the session's original directory if we're not already there
			if rec.Cwd != "" {
				if cwd, err := os.Getwd(); err == nil && cwd != rec.Cwd {
					if err := os.Chdir(rec.Cwd); err == nil {
						log.Printf("Resumed session cwd: %s", rec.Cwd)
					}
				}
			}
		}
	}

	// Resolve device ID early so the daemon can verify it matches
	resolvedDeviceID := *deviceID
	if resolvedDeviceID == "" {
		resolvedDeviceID = os.Getenv("GREENLIGHT_DEVICE_ID")
	}
	if resolvedDeviceID == "" {
		resolvedDeviceID = readConfigValue("device_id")
	}
	if err := ensureDaemon(resolvedDeviceID); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to start daemon: %v\n", err)
		os.Exit(1)
	}
	cwd, _ := os.Getwd()
	connectViaDaemon(*agentFlag, resolvedDeviceID, *project, *resume, cwd)
}

// startTranscriptStreamer polls for the agent's transcript file to appear,
// then spawns `greenlight stream --bridge` to tail it into the bridge file.
func startTranscriptStreamer(ctx context.Context, agent, relayID, agentSessionID, bridgePath, cwd string, notBefore time.Time, convIDOut *string) {
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
		p := deriveTranscriptPath(agent, agentSessionID, cwd)
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if agentSessionID != "" || info.ModTime().After(notBefore) {
			// Gemini creates a temp file during init that gets replaced
			// with the real session file. Don't accept a file without a
			// valid sessionId — keep polling until the real one appears.
			if agent == "gemini" && extractGeminiSessionID(p) == "" {
				continue
			}
			transcriptPath = p
			break
		}
	}
	// Re-derive after a short delay to handle the race where an old file gets
	// a brief mtime bump during init. If a newer file appeared, switch to it.
	// Skip for deterministic paths (agentSessionID set) and for Gemini (the
	// polling loop already validates the sessionId field, so the race is handled).
	if agentSessionID == "" && agent != "gemini" {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if p := deriveTranscriptPath(agent, "", cwd); p != "" && p != transcriptPath {
			log.Printf("Transcript: switching from %s to %s", transcriptPath, p)
			transcriptPath = p
		}
	}
	log.Printf("Transcript: found %s", transcriptPath)

	// Extract conversation ID from transcript path and persist the
	// conversation→relay mapping so resumed sessions reuse the same relay ID.
	// Copilot uses the parent directory name (session UUID) since all
	// transcripts are named "events.jsonl". Gemini stores the session UUID
	// in the JSON body (sessionId field). Other agents use the filename.
	var convID string
	switch agent {
	case "copilot":
		convID = filepath.Base(filepath.Dir(transcriptPath))
	case "gemini":
		convID = extractGeminiSessionID(transcriptPath)
	case "codex":
		// Codex filenames: rollout-YYYY-MM-DDTHH-MM-SS-UUID.jsonl
		// codex resume expects just the UUID portion.
		convID = extractCodexSessionID(transcriptPath)
	default:
		base := filepath.Base(transcriptPath)
		if ext := filepath.Ext(base); ext != "" {
			convID = strings.TrimSuffix(base, ext)
		}
	}
	if convID != "" {
		saveRelayID(convID, relayID)
		log.Printf("Transcript: saved relay mapping %s → %s", convID, relayID)
		if convIDOut != nil {
			*convIDOut = convID
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

// cleanupAgentFiles removes agent-specific files if no other greenlight
// connect sessions are active for the same agent and project dir.
func cleanupAgentFiles(agent, cwd string) {
	if hasOtherSessions(agent, cwd) {
		return
	}
	switch agent {
	case "gemini":
		removeGreenlightInstructions(filepath.Join(cwd, "GEMINI.md"))
	case "copilot":
		removeGreenlightInstructions(filepath.Join(cwd, ".github", "copilot-instructions.md"))
	case "cursor":
		removeGreenlightInstructions(filepath.Join(cwd, ".cursor", "rules", "greenlight.mdc"))
	case "codex":
		removeGreenlightInstructions(filepath.Join(cwd, "AGENTS.md"))
	}
	// Skills are installed under each agent's own root, namespaced under
	// _greenlight/. Idempotent — safe to call even if nothing was installed.
	removeSkills(agent, cwd)
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
	//
	// Copilot installs under ~/.copilot/pkg/ with varying directory layouts
	// across versions (e.g. darwin-arm64/1.0.2/, universal/1.0.5/). We glob
	// for all spawn-helper binaries matching the current architecture.
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	arch := "darwin-arm64"
	if runtime.GOARCH == "amd64" {
		arch = "darwin-x64"
	}

	// Copilot stores spawn-helper in multiple locations across versions:
	//   ~/.copilot/pkg/<variant>/<version>/prebuilds/<arch>/spawn-helper
	//   ~/Library/Caches/copilot/pkg/<variant>/<version>/prebuilds/<arch>/spawn-helper
	patterns := []string{
		filepath.Join(home, ".copilot", "pkg", "*", "*", "prebuilds", arch, "spawn-helper"),
		filepath.Join(home, "Library", "Caches", "copilot", "pkg", "*", "*", "prebuilds", arch, "spawn-helper"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, spawnHelper := range matches {
			if err := ensureDyldEntitlement(spawnHelper); err != nil {
				log.Printf("Interposition: spawn-helper %s: %v", spawnHelper, err)
			}
		}
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

	// Capture the original code signing identifier before re-signing.
	// macOS Keychain ACLs are tied to the identifier, so changing it
	// causes the binary to lose access to stored credentials (e.g.
	// Copilot auth tokens). Preserving the identifier across re-signs
	// keeps Keychain access working.
	var origIdentifier string
	idOut, err := exec.Command("codesign", "-d", "--verbose=2", binPath).CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(string(idOut), "\n") {
			if strings.HasPrefix(line, "Identifier=") {
				origIdentifier = strings.TrimPrefix(line, "Identifier=")
				break
			}
		}
	}

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

	signArgs := []string{"--sign", "-",
		"--entitlements", plistPath,
		"--options", "runtime",
	}
	if origIdentifier != "" {
		signArgs = append(signArgs, "--identifier", origIdentifier)
		log.Printf("Interposition: preserving identifier %s", origIdentifier)
	}
	signArgs = append(signArgs, binPath)
	signCmd := exec.Command("codesign", signArgs...)
	if out, err := signCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codesign sign: %s: %w", string(out), err)
	}

	// Remove quarantine xattr so Gatekeeper doesn't block the ad-hoc
	// signed binary. The original binary already passed Gatekeeper when
	// it was installed; our re-sign only adds entitlements.
	exec.Command("xattr", "-d", "com.apple.quarantine", binPath).Run()

	log.Printf("Interposition: re-signed %s successfully", binPath)
	return nil
}

// parseShebang is the pure core of the script-resolution logic. Given the
// leading bytes of a file, it returns the interpreter and its args if the
// file is a "#!" script. A "#!/usr/bin/env [-flags] interp [args]" line is
// unwrapped to the real interpreter. ok=false means the bytes are not a
// shebang, name no interpreter, or carry an empty line — callers should treat
// the file as a native binary. Split out from the file I/O so it can be fuzzed.
func parseShebang(header []byte) (interp string, interpArgs []string, ok bool) {
	if len(header) < 2 || header[0] != '#' || header[1] != '!' {
		return "", nil, false
	}
	line := string(header[2:])
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)

	parts := strings.Fields(line)
	if len(parts) == 0 {
		return "", nil, false
	}
	interp = parts[0]
	interpArgs = parts[1:]
	if filepath.Base(interp) == "env" {
		// Skip "env" and its flags (like -S); the first non-flag is the
		// real interpreter, and anything after it is interpreter args.
		for i, p := range parts[1:] {
			if !strings.HasPrefix(p, "-") {
				return p, parts[i+2:], true
			}
		}
		return "", nil, false // env with no interpreter
	}
	return interp, interpArgs, true
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

	header := make([]byte, 258) // "#!" + up to 256 bytes of shebang line
	n, _ := f.Read(header)
	interp, interpArgs, ok := parseShebang(header[:n])
	if !ok {
		return command, args // not a resolvable script
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

	header := make([]byte, 258) // "#!" + up to 256 bytes of shebang line
	n, _ := f.Read(header)
	interp, _, ok := parseShebang(header[:n])
	if !ok {
		return binPath, nil // not a resolvable script
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
func resolveCursorCommand(command string, args []string) (string, []string, error) {
	binPath, err := exec.LookPath(command)
	if err != nil {
		return "", nil, fmt.Errorf("cursor binary %q not found: %w", command, err)
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
		return "", nil, fmt.Errorf("cursor node not found at %s: %w", nodeBin, err)
	}
	if _, err := os.Stat(indexJS); err != nil {
		return "", nil, fmt.Errorf("cursor index.js not found at %s: %w", indexJS, err)
	}

	newArgs := []string{"--use-system-ca", indexJS}
	newArgs = append(newArgs, args...)

	// Set CURSOR_INVOKED_AS — the bash wrapper normally does this
	os.Setenv("CURSOR_INVOKED_AS", filepath.Base(binPath))

	log.Printf("Interposition: launching %s %s (bypassing cursor bash wrapper)", nodeBin, strings.Join(newArgs, " "))
	return nodeBin, newArgs, nil
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
