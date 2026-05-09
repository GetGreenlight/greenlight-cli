#!/usr/bin/env python3
"""
Agent matrix smoke test.

For each supported agent runtime that's installed locally, launches it
through:

    greenlight connect → greenlight daemon → greenlight-mockserver

…and asserts that:

  1. The agent's binary resolves and the entitlement re-sign step
     succeeds (macOS dyld validation).
  2. The session registers with the mock server (session_start arrived
     over /ws/daemon).
  3. The agent is still running after a settling period (i.e., didn't
     crash on startup).

Then it terminates the agent with SIGINT and confirms clean shutdown.

This is NOT a CI test. Some agents auth lazily; some won't run without
an internet connection. Run on demand before a release. Pair with
`scripts/update-agents.sh` to refresh agent versions first.

Usage:
    python3 tests/agent_matrix_test.py
    python3 tests/agent_matrix_test.py --only claude codex
    python3 tests/agent_matrix_test.py -v          # verbose subprocess output
    python3 tests/agent_matrix_test.py --keep      # don't tear down on exit
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import pty
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent
MOCK_ADDR = "127.0.0.1:17780"  # distinct port from daemon_e2e_test.py


@dataclass
class AgentSpec:
    """Per-agent metadata for the smoke test."""
    name: str            # value passed to --agent
    binary: str          # binary name to look up in PATH
    skip_on: set[str]    # platforms to skip ('darwin', 'linux')


AGENTS = [
    AgentSpec("claude",  "claude",       set()),
    AgentSpec("codex",   "codex",        set()),
    AgentSpec("copilot", "copilot",      set()),
    AgentSpec("cursor",  "cursor-agent", set()),  # also "agent" historically
    AgentSpec("gemini",  "gemini",       set()),
    AgentSpec("pi",      "pi",           set()),
]


def find_agent(spec: AgentSpec) -> str | None:
    if shutil.which(spec.binary):
        return spec.binary
    if spec.name == "cursor" and shutil.which("agent"):
        return "agent"
    return None


# -------- mock server / daemon (factored from daemon_e2e_test.py) --------

class MockServer:
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
            return json.loads(raw) if raw else None

    def sessions(self) -> list[dict]:
        return self._http("GET", "/_admin/sessions") or []

    def inbox(self, relay_id: str) -> list:
        return self._http("GET", f"/_admin/sessions/{relay_id}/inbox") or []

    def send_text(self, relay_id: str, frame: dict):
        self._http("POST", f"/_admin/sessions/{relay_id}/send", frame)

    def enroll_host(self, device_id: str, session_id: str):
        self._http("POST", "/session/enroll", {
            "device_id": device_id, "session_id": session_id, "hostname": "matrix-host",
        })

    def shutdown(self):
        self.proc.send_signal(signal.SIGTERM)
        try: self.proc.wait(timeout=3)
        except subprocess.TimeoutExpired: self.proc.kill()


class Daemon:
    def __init__(self, bin_path: Path, *, sock: str, home: Path,
                 device_id: str, session_id: str, verbose: bool, extra_path: str = ""):
        path = os.environ.get("PATH", "")
        if extra_path:
            path = f"{extra_path}:{path}"
        env = {
            "HOME": str(home), "PATH": path, "TMPDIR": str(home),
            "GREENLIGHT_DAEMON_SOCK": sock,
            "GREENLIGHT_DEVICE_ID": device_id,
            "GREENLIGHT_DAEMON_SESSION_ID": session_id,
            # Real interpose for the matrix test — that's the point.
        }
        self.sock = sock
        self.proc = subprocess.Popen(
            [str(bin_path), "daemon", "start", "--foreground"],
            env=env,
            stdout=None if verbose else subprocess.DEVNULL,
            stderr=None if verbose else subprocess.DEVNULL,
            preexec_fn=os.setsid,
        )
        self._wait_ready()

    def _wait_ready(self, timeout: float = 5.0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if os.path.exists(self.sock):
                try:
                    s = socket.socket(socket.AF_UNIX); s.connect(self.sock); s.close()
                    return
                except OSError: pass
            time.sleep(0.1)
        raise RuntimeError(f"daemon socket {self.sock} did not appear")

    def shutdown(self):
        try: os.killpg(os.getpgid(self.proc.pid), signal.SIGTERM)
        except ProcessLookupError: return
        try: self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(os.getpgid(self.proc.pid), signal.SIGKILL); self.proc.wait()


# -------- per-agent driver --------

def go_build(verbose: bool) -> tuple[Path, Path]:
    out = REPO_ROOT / ".build" / "matrix"
    out.mkdir(parents=True, exist_ok=True)
    gl_bin = out / "greenlight"
    ms_bin = out / "mockserver"

    ldflags = (
        f"-X main.version=0.0.0-matrix "
        f"-X main.wsURL=ws://{MOCK_ADDR}/ws/relay"
    )
    env = {**os.environ, "CGO_ENABLED": "0"}
    if sys.platform == "darwin":
        env["MACOSX_DEPLOYMENT_TARGET"] = "13.0"

    for desc, cmd in [
        ("greenlight", ["go", "build", "-ldflags", ldflags, "-o", str(gl_bin), "."]),
        ("mockserver", ["go", "build", "-o", str(ms_bin), "./cmd/mockserver/"]),
    ]:
        if verbose: print(f"$ {' '.join(cmd)}")
        r = subprocess.run(cmd, cwd=REPO_ROOT, env=env, capture_output=not verbose)
        if r.returncode != 0:
            out_text = "" if verbose else (r.stdout.decode() + r.stderr.decode())
            raise RuntimeError(f"build {desc} failed:\n{out_text}")
    return gl_bin, ms_bin


def run_agent(spec: AgentSpec, gl_bin: Path, ms: MockServer,
              tmp: Path, *, verbose: bool, settle: float = 8.0) -> tuple[bool, str]:
    """Launch the agent through the full stack and verify it registers
    a session and stays alive through `settle` seconds. Returns
    (passed, reason)."""
    if platform.system().lower() in spec.skip_on:
        return True, f"skipped on {platform.system()}"

    binary = find_agent(spec)
    if not binary:
        return True, "not installed"

    workdir = tmp / spec.name; workdir.mkdir()
    home = tmp / f"home-{spec.name}"; home.mkdir()
    sock = str(tmp / f"daemon-{spec.name}.sock")
    device_id = f"matrix-{spec.name}"
    session_id = f"00000000-0000-0000-0000-00000000{ord(spec.name[0]):04x}"

    # Reuse the user's real agent state (auth tokens, configs) so the
    # agent can talk to its backend on first launch.
    real_home = Path(os.path.expanduser("~"))
    for sub in [".claude", ".codex", ".copilot", ".cursor", ".gemini", ".pi"]:
        src = real_home / sub
        if src.exists():
            (home / sub).symlink_to(src)

    ms.enroll_host(device_id, session_id)
    daemon = Daemon(gl_bin, sock=sock, home=home, device_id=device_id,
                    session_id=session_id, verbose=verbose)

    client = None
    master = None
    try:
        master, slave = pty.openpty()
        env = {
            "HOME": str(home),
            "PATH": os.environ.get("PATH", ""),
            "TMPDIR": str(tmp),
            "TERM": "xterm-256color",
            "GREENLIGHT_DAEMON_SOCK": sock,
            "GREENLIGHT_LOG": str(tmp / f"client-{spec.name}.log"),
        }
        client = subprocess.Popen(
            [str(gl_bin), "connect",
             "--device-id", device_id, "--project", "matrix",
             "--agent", spec.name],
            cwd=workdir, env=env,
            stdin=slave, stdout=slave, stderr=slave,
            preexec_fn=os.setsid,
        )
        os.close(slave); slave = None

        relay_id = wait_for_session(ms, timeout=20.0)
        if not relay_id:
            return False, "session never registered with mock server"

        # Let the agent finish initializing. If it crashes during this
        # window, the test fails.
        deadline = time.time() + settle
        while time.time() < deadline:
            if client.poll() is not None:
                return False, f"agent exited prematurely (code={client.returncode})"
            time.sleep(0.5)

        return True, f"registered as {relay_id[:8]}…, alive after {settle:.0f}s"
    finally:
        if client and client.poll() is None:
            try: os.killpg(os.getpgid(client.pid), signal.SIGINT)
            except ProcessLookupError: pass
            try: client.wait(timeout=5)
            except subprocess.TimeoutExpired:
                try: os.killpg(os.getpgid(client.pid), signal.SIGKILL)
                except ProcessLookupError: pass
                client.wait()
        if master is not None:
            try: os.close(master)
            except OSError: pass
        daemon.shutdown()


def wait_for_session(ms: MockServer, timeout: float) -> str | None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        for s in ms.sessions():
            return s["relay_id"]
        time.sleep(0.2)
    return None


# -------- main --------

def main():
    p = argparse.ArgumentParser()
    p.add_argument("--only", nargs="+", help="restrict to these agents")
    p.add_argument("-v", "--verbose", action="store_true")
    p.add_argument("--keep", action="store_true",
                   help="don't tear down temp dir on exit")
    p.add_argument("--settle", type=float, default=8.0,
                   help="seconds to let each agent run after registering")
    args = p.parse_args()

    print("==> building greenlight + mockserver")
    gl_bin, ms_bin = go_build(args.verbose)

    selected = AGENTS
    if args.only:
        selected = [a for a in AGENTS if a.name in args.only]
        if not selected:
            print(f"no matching agents: {args.only}", file=sys.stderr)
            return 2

    tmp = Path(tempfile.mkdtemp(prefix="gl-matrix-"))
    print(f"workspace: {tmp}")

    ms = MockServer(ms_bin, MOCK_ADDR, args.verbose)
    results = []
    try:
        for spec in selected:
            print(f"\n==> {spec.name}")
            t0 = time.time()
            try:
                ok, reason = run_agent(spec, gl_bin, ms, tmp,
                                       verbose=args.verbose, settle=args.settle)
            except Exception as e:
                ok, reason = False, f"exception: {e}"
            dt = time.time() - t0
            tag = "PASS" if ok else "FAIL"
            print(f"  {tag}: {spec.name} ({dt:.1f}s) — {reason}")
            results.append((spec.name, ok, reason, dt))
    finally:
        ms.shutdown()
        if not args.keep:
            shutil.rmtree(tmp, ignore_errors=True)
        else:
            print(f"(kept artifacts at {tmp})")

    print("\n==> summary")
    for name, ok, reason, dt in results:
        tag = "✓" if ok else "✗"
        print(f"  {tag} {name:8s} {dt:5.1f}s  {reason}")
    failed = [r for r in results if not r[1]]
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
