#!/usr/bin/env python3
"""
End-to-end test of the daemon + mockserver + connect pipeline.

Builds greenlight-dev (wired to the mockserver) and greenlight-mockserver,
starts them, runs `greenlight connect` against a mock claude binary, and
exercises the list_skills control frame round-trip via the mockserver
admin API.

This is the canonical template for any Python-driven test that needs a
real agent process running through a real daemon talking to a fake
server. For raw interpose testing (without a daemon or server), see
tests/agent_test.py.

Usage:
    python3 tests/daemon_e2e_test.py
    python3 tests/daemon_e2e_test.py -v       # show subprocess output
    python3 tests/daemon_e2e_test.py --keep   # don't tear down on exit
"""

from __future__ import annotations

import argparse
import json
import os
import pty
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent
MOCK_ADDR = "127.0.0.1:17779"  # avoid clashing with the dev script's 7777


# -------- build --------

def go_build(verbose: bool) -> tuple[Path, Path, Path]:
    """Build greenlight-dev, greenlight-mockserver, and a mock claude.

    Returns (greenlight_bin, mockserver_bin, mock_claude_bin).
    """
    out = REPO_ROOT / ".build" / "e2e"
    out.mkdir(parents=True, exist_ok=True)
    gl_bin = out / "greenlight"
    ms_bin = out / "mockserver"
    claude_bin = out / "claude"

    ldflags = (
        f"-X main.version=0.0.0-e2e "
        f"-X main.wsURL=ws://{MOCK_ADDR}/ws/relay"
    )
    env = {**os.environ, "CGO_ENABLED": "0"}
    if sys.platform == "darwin":
        env["MACOSX_DEPLOYMENT_TARGET"] = "13.0"

    _run(["go", "build", "-ldflags", ldflags, "-o", str(gl_bin), "."],
         cwd=REPO_ROOT, env=env, verbose=verbose, label="build greenlight")
    _run(["go", "build", "-o", str(ms_bin), "./cmd/mockserver/"],
         cwd=REPO_ROOT, env=env, verbose=verbose, label="build mockserver")
    # mock_claude.go does `import "C"` to force dynamic linking (needed by the
    # Linux seccomp integration tests), so it must be built with cgo enabled.
    claude_env = {**env, "CGO_ENABLED": "1"}
    _run(["go", "build", "-o", str(claude_bin), "./testdata/mock_claude.go"],
         cwd=REPO_ROOT, env=claude_env, verbose=verbose, label="build mock_claude")
    return gl_bin, ms_bin, claude_bin


def _run(cmd, *, cwd, env, verbose, label):
    if verbose:
        print(f"$ {' '.join(cmd)}")
    r = subprocess.run(cmd, cwd=cwd, env=env, capture_output=not verbose)
    if r.returncode != 0:
        out = "" if verbose else (r.stdout.decode() + r.stderr.decode())
        raise RuntimeError(f"{label} failed:\n{out}")


# -------- mockserver client --------

class MockServer:
    """Subprocess wrapper around `greenlight-mockserver`. Talks to its
    admin HTTP API for session inspection and frame injection."""

    def __init__(self, bin_path: Path, addr: str, verbose: bool):
        self.addr = addr
        self.proc = subprocess.Popen(
            [str(bin_path), "--addr", addr] + (["-v"] if verbose else []),
            stdout=None if verbose else subprocess.DEVNULL,
            stderr=None if verbose else subprocess.DEVNULL,
        )
        self._wait_ready()

    def _wait_ready(self, timeout: float = 5.0):
        host, port = self.addr.split(":")
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                with socket.create_connection((host, int(port)), timeout=0.5):
                    return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError(f"mockserver did not come up on {self.addr}")

    def _http(self, method: str, path: str, body=None):
        url = f"http://{self.addr}{path}"
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method,
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            raw = resp.read()
            if not raw:
                return None
            return json.loads(raw)

    def sessions(self) -> list[dict]:
        return self._http("GET", "/_admin/sessions") or []

    def inbox(self, relay_id: str) -> list:
        return self._http("GET", f"/_admin/sessions/{relay_id}/inbox") or []

    def send_binary(self, relay_id: str, frame: dict):
        self._http("POST", f"/_admin/sessions/{relay_id}/send_binary", frame)

    def enroll_host(self, device_id: str, session_id: str):
        """Pre-enroll a host so the daemon WS can connect."""
        self._http("POST", "/session/enroll", {
            "device_id": device_id, "session_id": session_id,
            "hostname": "e2e-host",
        })

    def shutdown(self):
        self.proc.send_signal(signal.SIGTERM)
        try:
            self.proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.proc.kill()


# -------- daemon --------

class Daemon:
    """Subprocess wrapper around `greenlight daemon start --foreground`."""

    def __init__(self, bin_path: Path, *, sock: str, home: Path,
                 device_id: str, session_id: str, verbose: bool):
        env = {
            "HOME": str(home),
            "PATH": os.environ.get("PATH", ""),
            "TMPDIR": str(home),
            "GREENLIGHT_DAEMON_SOCK": sock,
            "GREENLIGHT_DEVICE_ID": device_id,
            "GREENLIGHT_DAEMON_SESSION_ID": session_id,
            "GREENLIGHT_DISABLE_INTERPOSE": "1",
        }
        self.sock = sock
        self.proc = subprocess.Popen(
            [str(bin_path), "daemon", "start", "--foreground"],
            env=env,
            stdout=None if verbose else subprocess.DEVNULL,
            stderr=None if verbose else subprocess.DEVNULL,
            preexec_fn=os.setsid,  # own process group so we can SIGTERM the tree
        )
        self._wait_ready()

    def _wait_ready(self, timeout: float = 5.0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if os.path.exists(self.sock):
                try:
                    s = socket.socket(socket.AF_UNIX)
                    s.connect(self.sock)
                    s.close()
                    return
                except OSError:
                    pass
            time.sleep(0.1)
        raise RuntimeError(f"daemon socket {self.sock} did not appear")

    def shutdown(self):
        try:
            os.killpg(os.getpgid(self.proc.pid), signal.SIGTERM)
        except ProcessLookupError:
            return
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(os.getpgid(self.proc.pid), signal.SIGKILL)
            self.proc.wait()


# -------- the test --------

def install_skill(workdir: Path, name: str):
    skill_dir = workdir / ".claude" / "skills" / "_greenlight" / name
    skill_dir.mkdir(parents=True, exist_ok=True)
    (skill_dir / "SKILL.md").write_text(
        f"---\nname: {name}\ndescription: e2e test skill\n---\nbody\n"
    )


def run_test(verbose: bool, keep: bool) -> int:
    print("[1/4] building binaries ...")
    gl_bin, ms_bin, claude_bin = go_build(verbose)

    tmp = Path(tempfile.mkdtemp(prefix="gl-e2e-"))
    workdir = tmp / "work"
    workdir.mkdir()
    install_skill(workdir, "demo-skill")

    sock = str(tmp / "daemon.sock")
    home = tmp / "home"
    home.mkdir()
    device_id = "e2e-dev"
    session_id = "00000000-0000-0000-0000-000000000001"

    print("[2/4] starting mockserver and daemon ...")
    ms = MockServer(ms_bin, MOCK_ADDR, verbose)
    ms.enroll_host(device_id, session_id)
    daemon = Daemon(gl_bin, sock=sock, home=home, device_id=device_id,
                    session_id=session_id, verbose=verbose)

    failures: list[str] = []
    client = None
    try:
        print("[3/4] launching greenlight connect with mock claude ...")
        # connect runs the agent in raw mode through a PTY. Allocate one
        # so it doesn't fail trying to set termios on stdin.
        master, slave = pty.openpty()
        client_env = {
            "HOME": str(home),
            "PATH": f"{claude_bin.parent}:{os.environ.get('PATH', '')}",
            "TMPDIR": str(tmp),
            "TERM": "xterm-256color",
            "MOCK_CLAUDE_OUTPUT": str(workdir / "claude-stdin.txt"),
            "GREENLIGHT_DAEMON_SOCK": sock,
            "GREENLIGHT_LOG": str(tmp / "client.log"),
        }
        client = subprocess.Popen(
            [str(gl_bin), "connect",
             "--device-id", device_id, "--project", "e2e-proj"],
            cwd=workdir, env=client_env,
            stdin=slave, stdout=slave, stderr=slave,
        )
        os.close(slave)

        # Wait for mock_claude to print its marker — at this point the
        # daemon has sent session_start over /ws/daemon and the mock
        # server has registered the session.
        if not _wait_for_pty(master, b"MOCK_CLAUDE_STARTED", 10.0):
            failures.append("mock_claude did not start within 10s")
            return _finish(client, master, ms, daemon, tmp, failures, keep, home)

        sessions = _wait_for_one_session(ms, 5.0)
        if not sessions:
            failures.append("no session registered with mockserver")
            return _finish(client, master, ms, daemon, tmp, failures, keep, home)

        relay_id = sessions[0]["relay_id"]
        print(f"      session relay_id = {relay_id}")

        print("[4/4] sending list_skills, awaiting skills_listed ...")
        ms.send_binary(relay_id, {"type": "list_skills", "relay_id": relay_id})

        reply = _wait_for_inbox_match(
            ms, relay_id,
            lambda f: f.get("type") == "skills_listed",
            timeout=5.0,
        )
        if reply is None:
            failures.append("skills_listed not received within 5s")
            return _finish(client, master, ms, daemon, tmp, failures, keep, home)

        if reply.get("relay_id") != relay_id:
            failures.append(f"relay_id mismatch: {reply.get('relay_id')!r} != {relay_id!r}")
        if reply.get("skills") != ["demo-skill"]:
            failures.append(f"skills mismatch: got {reply.get('skills')!r}, want ['demo-skill']")

        # Unblock mock_claude so the client exits cleanly.
        os.write(master, b"done\n")
        return _finish(client, master, ms, daemon, tmp, failures, keep, home)
    except Exception:
        if client is not None:
            client.kill()
        ms.shutdown()
        daemon.shutdown()
        raise


def _finish(client, master, ms, daemon, tmp, failures, keep, home):
    try:
        client.wait(timeout=10)
    except subprocess.TimeoutExpired:
        client.kill()
        client.wait()
    try:
        os.close(master)
    except OSError:
        pass
    daemon.shutdown()
    ms.shutdown()
    if failures:
        print("FAIL:")
        for f in failures:
            print(f"  - {f}")
        client_log = tmp / "client.log"
        if client_log.exists():
            print(f"\nclient log ({client_log}):")
            print(client_log.read_text())
        daemon_log = home / ".greenlight" / "daemon.log"
        if daemon_log.exists():
            print(f"\ndaemon log ({daemon_log}):")
            print(daemon_log.read_text())
    else:
        print("PASS: list_skills round-trip succeeded")
    if not keep:
        shutil.rmtree(tmp, ignore_errors=True)
    else:
        print(f"(kept artifacts at {tmp})")
    return 1 if failures else 0


def _wait_for_pty(master_fd: int, needle: bytes, timeout: float) -> bool:
    import select
    buf = b""
    deadline = time.time() + timeout
    while time.time() < deadline:
        rfds, _, _ = select.select([master_fd], [], [], 0.2)
        if master_fd in rfds:
            try:
                chunk = os.read(master_fd, 4096)
            except OSError:
                return False
            if not chunk:
                return False
            buf += chunk
            if needle in buf:
                return True
    return False


def _wait_for_one_session(ms: MockServer, timeout: float) -> list[dict]:
    deadline = time.time() + timeout
    while time.time() < deadline:
        s = ms.sessions()
        if len(s) == 1:
            return s
        time.sleep(0.1)
    return ms.sessions()


def _wait_for_inbox_match(ms: MockServer, relay_id: str, match, timeout: float):
    deadline = time.time() + timeout
    while time.time() < deadline:
        for raw in ms.inbox(relay_id):
            try:
                frame = json.loads(raw) if isinstance(raw, str) else raw
            except (TypeError, json.JSONDecodeError):
                continue
            if match(frame):
                return frame
        time.sleep(0.1)
    return None


def main():
    p = argparse.ArgumentParser()
    p.add_argument("-v", "--verbose", action="store_true")
    p.add_argument("--keep", action="store_true",
                   help="don't tear down temp dir on exit")
    args = p.parse_args()
    sys.exit(run_test(args.verbose, args.keep))


if __name__ == "__main__":
    main()
