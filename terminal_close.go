//go:build darwin || linux

package main

import (
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

// killedExitCode is a reserved exit code the Linux daemon-spawned shell
// wrapper (openTerminalLinux) checks for to distinguish an app-initiated kill
// from a normal agent exit. Only ever used on the Killed path (issue #273),
// so ordinary exit codes are unaffected.
const killedExitCode = 99

// spawnTerminalCloseHelper arranges for the terminal window hosting this
// client process to close once the client itself has actually exited. Only
// implemented for macOS Terminal.app; on Linux the equivalent behavior is
// driven by killedExitCode + openTerminalLinux's shell wrapper instead, so
// this is a no-op there.
//
// The client cannot close its own window here: Terminal.app's "ask before
// closing" preference fires when closing a window whose foreground job is
// still alive — which is exactly true of this process while it runs. Instead
// a detached helper (mirroring the transcript streamer's re-exec pattern,
// see connect.go) polls for this process's pid to disappear, then closes the
// matched window by tty — but only when it's the window's sole tab (see
// closeTerminalTabByTTY): Terminal.app's AppleScript dictionary has no way to
// close a single tab out of a window with siblings, so a window with other
// live tabs is left alone rather than risk tearing down unrelated sessions.
func spawnTerminalCloseHelper() {
	if runtime.GOOS != "darwin" {
		return
	}

	tty, err := controllingTTY()
	if err != nil {
		log.Printf("greenlight: cannot resolve controlling tty, skipping terminal close: %v", err)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("greenlight: cannot resolve executable, skipping terminal close: %v", err)
		return
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	cmd := exec.Command(exePath, "internal-close-terminal",
		"--tty", tty,
		"--pid", strconv.Itoa(os.Getpid()),
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		log.Printf("greenlight: failed to spawn terminal-close helper: %v", err)
		return
	}
	cmd.Process.Release()
}

// controllingTTY resolves the path of this process's controlling terminal
// (e.g. /dev/ttys003 on macOS), matching what AppleScript's `tty of <tab>`
// reports. Delegates the actual fd-to-path resolution to a per-OS helper:
// unlike /proc/self/fd on Linux, macOS's /dev/fd/<n> entries are not real
// symlinks (readlink on them fails with EINVAL), so os.Readlink can never
// resolve them there.
func controllingTTY() (string, error) {
	for _, fd := range []int{0, 1, 2} {
		if path, err := ttyPathFromFD(fd); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no controlling tty found")
}

// runInternalCloseTerminal is the detached helper's entry point
// (`greenlight internal-close-terminal --tty T --pid P`), spawned by
// spawnTerminalCloseHelper. It waits for pid P (the client process that
// spawned it) to exit, then closes the Terminal.app tab matching tty T.
// Bounded so a hung client never leaves the helper spinning forever. Hidden
// from the usual subcommand help text — internal use only.
func runInternalCloseTerminal(args []string) {
	var tty string
	var pid int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tty":
			if i+1 < len(args) {
				i++
				tty = args[i]
			}
		case "--pid":
			if i+1 < len(args) {
				i++
				pid, _ = strconv.Atoi(args[i])
			}
		}
	}
	if tty == "" || pid <= 0 {
		return
	}

	const pollInterval = 100 * time.Millisecond
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break // ESRCH — client process has exited
		}
		time.Sleep(pollInterval)
	}

	outcome, err := closeTerminalTabByTTY(tty)
	if err != nil {
		log.Printf("greenlight: failed to close terminal window for tty %s: %v", tty, err)
		return
	}
	log.Printf("greenlight: terminal close for tty %s: %s", tty, outcome)
}

// closeTerminalTabByTTY closes the Terminal.app window matching tty, but only
// when the killed session's tab is the window's sole tab. Terminal.app's
// AppleScript dictionary has no "close" command on its tab class at all
// (confirmed against Terminal's own sdef — every attempt to close an
// individual tab object errors with "doesn't understand the close message"),
// so there is no way to tear down one tab while leaving sibling tabs in the
// same window alone. Rather than close the whole window and take out
// unrelated live sessions (issue #273's round-3 concern), a window with
// sibling tabs is left untouched — "skipped-siblings" — and only the common
// single-tab-per-window case actually closes.
func closeTerminalTabByTTY(tty string) (string, error) {
	escaped := escapeAppleScriptString(tty)
	script := fmt.Sprintf(`tell application "Terminal"
	set targetWin to missing value
	repeat with w in windows
		repeat with t in tabs of w
			if tty of t is "%s" then set targetWin to w
		end repeat
	end repeat
	if targetWin is missing value then
		return "not-found"
	else if (count of tabs of targetWin) = 1 then
		close targetWin
		return "closed"
	else
		return "skipped-siblings"
	end if
end tell`, escaped)
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
