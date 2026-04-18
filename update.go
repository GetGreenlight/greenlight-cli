//go:build darwin || linux

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const githubRepo = "GetGreenlight/greenlight-cli"

func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "Check for updates without downloading")
	force := fs.Bool("force", false, "Skip confirmation for active sessions")
	fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "greenlight update: unexpected argument %q\nRun 'greenlight update --help' for usage.\n", fs.Arg(0))
		os.Exit(1)
	}

	currentVersion := version
	if currentVersion == "" {
		currentVersion = "dev"
	}

	// If updateURL is set (build-time override), skip version check and download directly
	if updateURL != "" {
		fmt.Fprintf(os.Stderr, "greenlight: updating from %s\n", updateURL)
		if err := applyUpdate(updateURL, "override", *force); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: update failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "greenlight: updated from %s\n", updateURL)
		return
	}

	fmt.Fprintf(os.Stderr, "greenlight: checking for updates...\n")

	latestTag, err := fetchLatestTag()
	if err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: failed to check for updates: %v\n", err)
		os.Exit(1)
	}

	// Normalize: strip leading "v" for comparison
	latestVersion := strings.TrimPrefix(latestTag, "v")
	currentNorm := strings.TrimPrefix(currentVersion, "v")

	if latestVersion == currentNorm {
		fmt.Fprintf(os.Stderr, "greenlight: already up to date (%s)\n", currentVersion)
		return
	}

	if *check {
		fmt.Fprintf(os.Stderr, "greenlight: update available: %s → %s\n", currentVersion, latestVersion)
		return
	}

	fmt.Fprintf(os.Stderr, "greenlight: updating %s → %s\n", currentVersion, latestVersion)

	assetName := fmt.Sprintf("greenlight-%s-%s", runtime.GOOS, runtime.GOARCH)
	downloadURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
		githubRepo, latestTag, assetName)

	if err := applyUpdate(downloadURL, latestVersion, *force); err != nil {
		fmt.Fprintf(os.Stderr, "greenlight: update failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "greenlight: updated to %s\n", latestVersion)
}

// fetchLatestTag resolves the latest release tag from GitHub.
func fetchLatestTag() (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirects
		},
	}

	resp, err := client.Head(fmt.Sprintf("https://github.com/%s/releases/latest", githubRepo))
	if err != nil {
		return "", fmt.Errorf("failed to fetch latest release: %w", err)
	}
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no redirect from GitHub releases/latest")
	}

	// Location: https://github.com/GetGreenlight/greenlight-cli/releases/tag/v1.0.0
	idx := strings.LastIndex(loc, "/tag/")
	if idx < 0 {
		return "", fmt.Errorf("unexpected redirect URL: %s", loc)
	}
	tag := loc[idx+5:]
	if tag == "" {
		return "", fmt.Errorf("empty tag in redirect URL: %s", loc)
	}
	return tag, nil
}

// applyUpdate downloads the binary and replaces the current one.
// If the daemon is running with active sessions and force is false,
// the user is prompted before terminating them.
func applyUpdate(downloadURL, newVersion string, force bool) error {
	// Resolve current binary path
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve executable: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("cannot resolve symlinks: %w", err)
	}

	// Download to a temp file in the same directory (needed for atomic rename)
	dir := filepath.Dir(exePath)
	tmpFile, err := os.CreateTemp(dir, ".greenlight-update-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // cleanup on failure

	// Get a reader for the new binary (file:// or HTTP)
	var src io.ReadCloser
	if strings.HasPrefix(downloadURL, "file://") {
		localPath := strings.TrimPrefix(downloadURL, "file://")
		f, err := os.Open(localPath)
		if err != nil {
			tmpFile.Close()
			return fmt.Errorf("cannot open local file: %w", err)
		}
		src = f
	} else {
		client := &http.Client{Timeout: 120 * time.Second}
		resp, err := client.Get(downloadURL)
		if err != nil {
			tmpFile.Close()
			return fmt.Errorf("download failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			tmpFile.Close()
			return fmt.Errorf("download failed: HTTP %d (url: %s)", resp.StatusCode, downloadURL)
		}
		src = resp.Body
	}
	defer src.Close()

	// Write and compute SHA-256 simultaneously
	hash := sha256.New()
	writer := io.MultiWriter(tmpFile, hash)
	if _, err := io.Copy(writer, src); err != nil {
		tmpFile.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	tmpFile.Close()

	checksum := hex.EncodeToString(hash.Sum(nil))
	log.Printf("update: downloaded %s (sha256: %s)", downloadURL, checksum)

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	// Verify code signature on macOS
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("codesign", "--verify", tmpPath).CombinedOutput(); err != nil {
			log.Printf("update: codesign verify: %s", string(out))
			// Don't fail — dev builds aren't signed
		}
	}

	// Verify the downloaded binary is a valid greenlight binary and check version
	verifyOut, err := exec.Command(tmpPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("downloaded binary failed verification (%s version): %w", tmpPath, err)
	}
	// Extract version (vN.N.N) from the output
	newVer := extractVersion(string(verifyOut))
	currentVer := extractVersion(version)
	if newVer == "" {
		return fmt.Errorf("downloaded binary has no version set")
	}
	if currentVer != "" && newVer == currentVer {
		return fmt.Errorf("already up to date (%s)", currentVer)
	}
	fmt.Fprintf(os.Stderr, "greenlight: verified new binary: %s → %s\n", currentVer, newVer)

	// Shut down daemon if running (after download + verify so we don't
	// disrupt sessions unnecessarily if the update would fail anyway)
	daemonWasRunning := isDaemonRunning()
	if daemonWasRunning {
		if err := shutdownDaemonForUpdate(force); err != nil {
			return err
		}
	}

	// Atomic rename (works on both macOS and Linux even while the old binary is running)
	if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("cannot replace binary: %w", err)
	}

	// Restart daemon if it was running (ensureDaemon re-enrolls)
	if daemonWasRunning {
		fmt.Fprintf(os.Stderr, "greenlight: restarting daemon...\n")
		if err := ensureDaemon(""); err != nil {
			fmt.Fprintf(os.Stderr, "greenlight: daemon restart failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "greenlight: run 'greenlight daemon start' to restart manually\n")
		}
	}

	return nil
}

// shutdownDaemonForUpdate sends an update_shutdown IPC message to the daemon.
// If there are active instances and force is false, it prompts the user.
func shutdownDaemonForUpdate(force bool) error {
	instances, err := sendUpdateShutdown(force)
	if err != nil {
		return err
	}

	if instances == nil {
		// Daemon accepted shutdown (no active instances or force was true)
		return waitForDaemonExit()
	}

	// Active instances — show them and prompt
	fmt.Fprintf(os.Stderr, "greenlight: %d active instance(s) will be terminated:\n", len(instances))
	for _, s := range instances {
		fmt.Fprintf(os.Stderr, "  %-10s %-8s %-20s %s\n",
			s.AIAgentInstanceID[:min(10, len(s.AIAgentInstanceID))], s.Agent, s.Project, s.Cwd)
	}
	fmt.Fprintf(os.Stderr, "Continue? [y/N] ")

	var answer string
	fmt.Scanln(&answer)
	if answer != "y" && answer != "Y" {
		return fmt.Errorf("update cancelled")
	}

	// User confirmed — send again with force
	_, err = sendUpdateShutdown(true)
	if err != nil {
		return err
	}
	return waitForDaemonExit()
}

// sendUpdateShutdown sends the update_shutdown IPC message and returns
// the active instances list (nil if the daemon accepted shutdown).
func sendUpdateShutdown(force bool) ([]instanceInfo, error) {
	conn, err := net.DialTimeout("unix", daemonSockPath(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer conn.Close()

	msg, _ := json.Marshal(ipcRequest{Type: "update_shutdown", Force: force})
	msg = append(msg, '\n')
	conn.Write(msg)

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("daemon did not respond: %w", err)
	}

	var resp ipcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("invalid daemon response: %w", err)
	}

	switch resp.Type {
	case "ok":
		fmt.Fprintf(os.Stderr, "greenlight: daemon shutting down for update...\n")
		return nil, nil
	case "active_instances":
		return resp.Instances, nil
	case "error":
		return nil, fmt.Errorf("daemon error: %s", resp.Message)
	default:
		return nil, fmt.Errorf("unexpected response: %s", resp.Type)
	}
}

var versionRe = regexp.MustCompile(`v\d+\.\d+(?:\.\d+)?`)

// extractVersion finds a vN.N.N version string in the input.
func extractVersion(s string) string {
	return versionRe.FindString(s)
}

// waitForDaemonExit polls until the daemon has stopped.
func waitForDaemonExit() error {
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isDaemonRunning() {
			return nil
		}
	}
	return fmt.Errorf("daemon did not exit in time")
}
