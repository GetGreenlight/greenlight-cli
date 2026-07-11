//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Seccomp ioctl commands — architecture-dependent due to struct sizes.
// struct seccomp_notif is 80 bytes, struct seccomp_notif_resp is 24 bytes.
const (
	// _IOWR('!', 0, struct seccomp_notif) — read+write, 80 bytes
	seccompIoctlNotifRecv = 0xc0502100
	// _IOWR('!', 1, struct seccomp_notif_resp) — read+write, 24 bytes
	seccompIoctlNotifSend = 0xc0182101
	// _IOW('!', 2, __u64) — write, 8 bytes
	seccompIoctlNotifIDValid = 0x40082102

	seccompUserNotifFlagContinue = 0x00000001

	sysOpenat    = 257
	sysRenameat  = 264
	sysRenameat2 = 316
	atFDCWD      = -100

	oWRONLY = 0x1
	oRDWR   = 0x2
	oCREAT  = 0x40
	oTRUNC  = 0x200
)

// seccompNotif matches the kernel's struct seccomp_notif (80 bytes).
type seccompNotif struct {
	ID    uint64
	PID   uint32
	Flags uint32
	Data  seccompData
}

// seccompData matches struct seccomp_data.
type seccompData struct {
	Nr                 int32
	Arch               uint32
	InstructionPointer uint64
	Args               [6]uint64
}

// seccompNotifResp matches struct seccomp_notif_resp (24 bytes).
type seccompNotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

// Permission cache: recently-allowed paths are auto-approved to avoid
// re-prompting for retries of the same write operation.
var (
	seccompPermCache   = make(map[string]time.Time)
	seccompPermCacheMu sync.Mutex
)

func seccompPermCacheAllowed(path string) bool {
	seccompPermCacheMu.Lock()
	defer seccompPermCacheMu.Unlock()
	if t, ok := seccompPermCache[path]; ok && time.Since(t) < 30*time.Second {
		return true
	}
	return false
}

func seccompPermCacheStore(path string) {
	seccompPermCacheMu.Lock()
	seccompPermCache[path] = time.Now()
	seccompPermCacheMu.Unlock()
}

// runSeccompSupervisor receives seccomp notifications for write-mode openat,
// renameat, and renameat2 syscalls. It reads the path from /proc/<pid>/mem,
// applies permission checks, and allows or denies each syscall.
func runSeccompSupervisor(notifFd int, agent string, ir *interposeRelay) {
	log.Printf("Seccomp supervisor started on fd %d", notifFd)

	// Lock OS thread — the blocking ioctl needs a dedicated thread
	// so Go's runtime scheduler isn't starved.
	go func() {
		seccompSupervisorLoop(notifFd, agent, ir)
	}()
}

func seccompSupervisorLoop(notifFd int, agent string, ir *interposeRelay) {
	for {
		var notif seccompNotif
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
			uintptr(notifFd),
			uintptr(seccompIoctlNotifRecv),
			uintptr(unsafe.Pointer(&notif)))
		if errno != 0 {
			if errno == syscall.EINTR {
				continue
			}
			log.Printf("Seccomp supervisor: recv error: %v, exiting", errno)
			return
		}

		allow := handleSeccompNotif(notifFd, &notif, agent, ir)

		var resp seccompNotifResp
		resp.ID = notif.ID
		if allow {
			resp.Flags = seccompUserNotifFlagContinue
		} else {
			resp.Val = -1
			resp.Error = -int32(syscall.EACCES)
		}

		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL,
			uintptr(notifFd),
			uintptr(seccompIoctlNotifSend),
			uintptr(unsafe.Pointer(&resp)))
		if errno != 0 && errno != syscall.ENOENT {
			log.Printf("Seccomp supervisor: send error: %v", errno)
		}
	}
}

// handleSeccompNotif processes a single notification. Returns true to allow.
func handleSeccompNotif(notifFd int, notif *seccompNotif, agent string, ir *interposeRelay) bool {
	switch int(notif.Data.Nr) {
	case sysOpenat:
		return handleSeccompOpenat(notifFd, notif, agent, ir)
	case sysRenameat, sysRenameat2:
		return handleSeccompRename(notifFd, notif, agent, ir)
	default:
		return true
	}
}

func handleSeccompOpenat(notifFd int, notif *seccompNotif, agent string, ir *interposeRelay) bool {
	pid := notif.PID
	dirfd := int32(notif.Data.Args[0])
	pathPtr := notif.Data.Args[1]
	flags := uint32(notif.Data.Args[2])

	// Read the path from the process's memory
	path, err := readStringFromProc(pid, pathPtr)
	if err != nil {
		log.Printf("Seccomp: failed to read path from pid %d, denying: %v", pid, err)
		return false
	}

	// Resolve relative paths
	if !filepath.IsAbs(path) {
		base := ""
		if dirfd == int32(atFDCWD) {
			base, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		} else {
			base, _ = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, dirfd))
		}
		if base != "" {
			path = filepath.Join(base, path)
		}
	}

	// Canonicalize before classifying (issue #241) — resolves ".." and
	// symlinks so a raw prefix trick (e.g. "/tmp/../Users/me/.ssh/...")
	// can't spoof seccompIsTempPath/seccompIsSystemPath/etc. Failure fails
	// closed: skip the auto-allow short-circuit below and always fall
	// through to a permission prompt.
	classifyPath := path
	canonOk := false
	if canon, ok := seccompCanonicalize(pid, path); ok {
		classifyPath = canon
		canonOk = true
	}

	// Quick classification — auto-allow paths that don't need permission
	if canonOk && !seccompNeedsWritePermission(classifyPath, flags) {
		return true
	}

	// Resolve .tmp intermediate to real target path.
	// Pattern: "<real-path>.tmp.<pid>.<timestamp>"
	displayPath := classifyPath
	if idx := strings.Index(displayPath, ".tmp."); idx >= 0 {
		displayPath = displayPath[:idx]
	}

	// Check permission cache — avoid re-prompting for the same file
	if seccompPermCacheAllowed(displayPath) {
		return true
	}

	// Verify notification is still valid (TOCTOU check)
	if !seccompNotifValid(notifFd, notif.ID) {
		return true
	}

	// Determine tool name based on the real target path
	toolName := "Write"
	if _, err := os.Stat(displayPath); err == nil {
		toolName = "Edit"
	}
	toolInput := map[string]interface{}{"file_path": displayPath}

	log.Printf("Seccomp: %s %s (pid %d)", toolName, displayPath, pid)

	return seccompRequestPermission(toolName, toolInput, displayPath, agent, ir)
}

func handleSeccompRename(notifFd int, notif *seccompNotif, agent string, ir *interposeRelay) bool {
	pid := notif.PID
	newdirfd := int32(notif.Data.Args[2])
	newpathPtr := notif.Data.Args[3]

	newPath, err := readStringFromProc(pid, newpathPtr)
	if err != nil {
		log.Printf("Seccomp: failed to read rename path from pid %d, denying: %v", pid, err)
		return false
	}

	if !filepath.IsAbs(newPath) {
		base := ""
		if newdirfd == int32(atFDCWD) {
			base, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		} else {
			base, _ = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, newdirfd))
		}
		if base != "" {
			newPath = filepath.Join(base, newPath)
		}
	}

	// Canonicalize the target before classifying (issue #241) — see
	// handleSeccompOpenat. oldPath below is display/diff-only, not gating,
	// so it's left uncanonicalized.
	classifyPath := newPath
	canonOk := false
	if canon, ok := seccompCanonicalize(pid, newPath); ok {
		classifyPath = canon
		canonOk = true
	}

	if canonOk && !seccompNeedsRenamePermission(classifyPath) {
		return true
	}

	if !seccompNotifValid(notifFd, notif.ID) {
		return true
	}

	// Read old path for context
	olddirfd := int32(notif.Data.Args[0])
	oldpathPtr := notif.Data.Args[1]
	oldPath, _ := readStringFromProc(pid, oldpathPtr)
	if oldPath != "" && !filepath.IsAbs(oldPath) {
		base := ""
		if olddirfd == int32(atFDCWD) {
			base, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		} else {
			base, _ = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, olddirfd))
		}
		if base != "" {
			oldPath = filepath.Join(base, oldPath)
		}
	}

	// Determine tool name (edit if target exists, write if new)
	toolName := "Write"
	toolInput := map[string]interface{}{"file_path": classifyPath}
	if _, err := os.Stat(classifyPath); err == nil {
		toolName = "Edit"
		if oldPath != "" {
			oldStr, newStr := computeRenameEdit(classifyPath, oldPath)
			if oldStr != "" || newStr != "" {
				toolInput["old_string"] = oldStr
				toolInput["new_string"] = newStr
			}
		}
	}

	log.Printf("Seccomp: rename → %s %s (pid %d)", toolName, classifyPath, pid)

	return seccompRequestPermission(toolName, toolInput, classifyPath, agent, ir)
}

// seccompRequestPermission sends a permission request through the same
// racePermission path used by LD_PRELOAD interpose requests.
func seccompRequestPermission(toolName string, toolInput map[string]interface{}, path, agent string, ir *interposeRelay) bool {
	relay := ir.GetRelay()

	if relay == nil || relay.wsConn == nil {
		log.Printf("Seccomp: no relay available, denying %s %s", toolName, path)
		return false
	}

	payload := map[string]interface{}{
		"agent":           agent,
		"hook_event_name": "PermissionRequest",
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"project_file":    seccompIsProjectFile(path),
	}

	resp := racePermission(relay, toolName, toolInput, payload)

	if resp.Allow {
		seccompPermCacheStore(path)
	} else {
		writePermissionDenied(path)
	}

	return resp.Allow
}

// writePermissionDenied writes the standard denial message. This is best-effort.
func writePermissionDenied(path string) {
	// The denied process is blocked in seccomp — we can't easily write to its stderr
	// since we'd need to identify which fd is stderr. The EACCES return is sufficient;
	// the agent instruction files teach agents to handle permission errors.
}

// seccompNotifValid checks if the notification is still valid (TOCTOU protection).
func seccompNotifValid(notifFd int, id uint64) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(notifFd),
		uintptr(seccompIoctlNotifIDValid),
		uintptr(unsafe.Pointer(&id)))
	return errno == 0
}

// readStringFromProc reads a null-terminated string from /proc/<pid>/mem.
func readStringFromProc(pid uint32, addr uint64) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, 4096)
	n, err := f.ReadAt(buf, int64(addr))
	if n == 0 {
		return "", fmt.Errorf("read 0 bytes: %w", err)
	}

	// Find null terminator
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf[:n]), nil
}

// seccompCanonicalize resolves ".." traversal and symlinks in an absolute
// path as seen from pid's filesystem view, so a raw prefix trick (e.g.
// "/tmp/../Users/me/.ssh/id_rsa") can't spoof seccompIsTempPath/
// seccompIsSystemPath/etc (issue #241). Returns ("", false) on failure.
// Callers MUST fail closed (always require a permission prompt) rather
// than classifying the raw path. The actual resolution logic
// (seccompCanonicalizeUnderRoot) lives in pathcanon.go, a build-tag-free
// file, so it's directly unit-testable on any platform.
func seccompCanonicalize(pid uint32, rawPath string) (string, bool) {
	if rawPath == "" || !filepath.IsAbs(rawPath) {
		return "", false
	}
	root := fmt.Sprintf("/proc/%d/root", pid)
	return seccompCanonicalizeUnderRoot(root, rawPath)
}

// --- Path classification (mirrors C-side logic) ---

func seccompIsSystemPath(path string) bool {
	prefixes := []string{"/System/", "/Library/", "/usr/", "/dev/", "/etc/",
		"/var/", "/sbin/", "/bin/", "/opt/", "/Applications/",
		"/private/", "/sys/", "/proc/", "/run/"}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func seccompIsAgentInternal(path string) bool {
	patterns := []string{"/.claude/", "/.local/share/claude/", "/.local/state/claude/",
		"/Library/Caches/claude", "/Library/Application Support/Claude",
		"/.cursor/", "/.local/share/cursor-agent/", "/Library/Caches/cursor",
		"/.codex/", "/.copilot/", "/Library/Caches/copilot",
		"/.gemini/", "/.pi/"}
	for _, p := range patterns {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

func seccompIsTempPath(path string) bool {
	return strings.HasPrefix(path, "/tmp/") || strings.HasPrefix(path, "/private/tmp/")
}

func seccompIsDotfile(path string) bool {
	idx := 0
	for {
		pos := strings.Index(path[idx:], "/.")
		if pos < 0 {
			return false
		}
		pos += idx
		if pos+2 < len(path) && path[pos+2] != '.' && path[pos+2] != '/' {
			return true
		}
		idx = pos + 2
		if idx >= len(path) {
			return false
		}
	}
}

// seccompProjectDir is set by connect.go before the interpose socket starts.
var seccompProjectDir string

func seccompIsProjectFile(path string) bool {
	if seccompProjectDir == "" {
		return false
	}
	if !strings.HasPrefix(path, seccompProjectDir) {
		return false
	}
	rest := path[len(seccompProjectDir):]
	if rest != "" && rest[0] != '/' {
		return false
	}
	return !seccompIsDotfile(path)
}

func seccompNeedsWritePermission(path string, flags uint32) bool {
	isWrite := (flags & (oWRONLY | oRDWR | oCREAT | oTRUNC)) != 0
	if !isWrite {
		return false
	}
	if path == "" {
		return false
	}
	if seccompIsSystemPath(path) {
		return false
	}
	if seccompIsAgentInternal(path) {
		return false
	}
	if seccompIsTempPath(path) {
		return false
	}
	if seccompIsDotfile(path) {
		return false
	}
	// For seccomp: DO NOT skip .tmp intermediates — Bun/Zig writes to .tmp then
	// renames via direct syscall that may not be caught. Gate the .tmp open instead.
	// Extract the real target path from the .tmp pattern: "path.tmp.PID.TIMESTAMP"
	return true
}

func seccompNeedsRenamePermission(newPath string) bool {
	if newPath == "" {
		return false
	}
	if seccompIsSystemPath(newPath) {
		return false
	}
	if seccompIsAgentInternal(newPath) {
		return false
	}
	if seccompIsTempPath(newPath) {
		return false
	}
	if seccompIsDotfile(newPath) {
		return false
	}
	if strings.Contains(newPath, ".tmp.") {
		return false
	}
	return true
}

// Ensure the seccomp struct sizes match kernel expectations.
func init() {
	if unsafe.Sizeof(seccompNotif{}) != 80 {
		panic(fmt.Sprintf("seccompNotif size mismatch: got %d, want 80", unsafe.Sizeof(seccompNotif{})))
	}
	if unsafe.Sizeof(seccompNotifResp{}) != 24 {
		panic(fmt.Sprintf("seccompNotifResp size mismatch: got %d, want 24", unsafe.Sizeof(seccompNotifResp{})))
	}
}
