//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupCLIShim(t *testing.T) {
	dir, cleanup := setupCLIShim("test-relay-id")
	if dir == "" {
		t.Fatal("setupCLIShim returned empty dir")
	}
	t.Cleanup(cleanup)

	link := filepath.Join(dir, "greenlight")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("greenlight symlink missing: %v", err)
	}
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if target != exe {
		t.Errorf("symlink target = %q, want running test binary %q", target, exe)
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("shim dir not removed after cleanup: %v", err)
	}
}

func TestPrependPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	if got := prependPATH("/a", ""); got != "/a" {
		t.Errorf("empty existing: got %q", got)
	}
	if got := prependPATH("/a", "/b"+sep+"/c"); got != "/a"+sep+"/b"+sep+"/c" {
		t.Errorf("prepend: got %q", got)
	}
}

func TestGreenlightDirAndSock(t *testing.T) {
	orig := buildID
	t.Cleanup(func() { buildID = orig })

	buildID = ""
	if greenlightDirName() != ".greenlight" {
		t.Errorf("prod dir name: got %q", greenlightDirName())
	}
	if daemonSockName() != "greenlight-daemon.sock" {
		t.Errorf("prod sock name: got %q", daemonSockName())
	}

	buildID = "dev"
	if greenlightDirName() != ".greenlight-dev" {
		t.Errorf("dev dir name: got %q", greenlightDirName())
	}
	if daemonSockName() != "greenlight-daemon-dev.sock" {
		t.Errorf("dev sock name: got %q", daemonSockName())
	}
}
