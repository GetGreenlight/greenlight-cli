#!/usr/bin/env python3
"""
Agent-scenario test: drive a real agent through the full stack and
verify a tool-call round-trip end to end.

For each installed agent the harness:

  1. Snapshots the agent's transcript dir + ~/.greenlight/ state so
     anything the test writes can be reverted at teardown.
  2. Pre-trusts the test workdir in the agent's trust file so the
     interactive "trust this folder?" prompt doesn't block input
     (claude/codex/copilot/gemini all gate on this; cursor/pi don't).
     The trust file is restored byte-for-byte at teardown.
  3. Boots greenlight-mockserver + daemon and launches the agent
     through `greenlight connect` (real HOME — sandboxed HOMEs trip
     install-integrity checks in claude/cursor/codex).
  3. Once the session registers, sends a prompt over the relay WS that
     asks the agent to create a uniquely-named .txt file in the
     workdir, allows the resulting Write/Bash permission request, and
     waits for the file to appear.
  4. Sends a follow-up prompt asking for the file to be deleted, allows
     the second permission request, and waits for the file to vanish.
  5. Sends a kill control frame and asserts the agent exits.

The plumbing — prompt injection through the mock-server admin API,
permission auto-allow keyed by request_id, transcript/sessions snapshot
+ restore, control-frame teardown — is correct. The flaky bit is
prompt-engineering each agent into the desired tool call: this script
asks bluntly ("Create a file at <path> containing 'hi'") and that
works well for some models and tunes for others. Treat the harness as
a launching pad — adjust the prompts per-agent as vendors change UX.

Known issues at time of writing:
  - cursor needs interactive `agent --login` once before the harness
    can run it; no folder trust gate.
  - claude on macOS refuses to authenticate when launched from inside
    another claude session (auth_failed in 13ms, synthetic "Not logged
    in" reply). Confirmed: same harness from a fresh terminal works;
    from inside a claude session over SSH to a Linux box also works.
    Inheritance is via macOS audit-session attributes carried through
    process ancestry, not env. The harness auto-skips claude on macOS
    when CLAUDECODE=1; on Linux it always runs. Two ways to test
    claude under any context: (a) fresh Terminal on macOS, (b) a
    Linux machine.

Not a CI test. Real backends, real auth, real cost. Run on demand.

Usage:
    python3 tests/agent_scenario_test.py                  # all installed
    python3 tests/agent_scenario_test.py --only claude    # subset
    python3 tests/agent_scenario_test.py -v --keep        # debug
"""

from __future__ import annotations

import argparse
import base64
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
import threading
import time
import urllib.request
import uuid
from dataclasses import dataclass, field
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent
MOCK_ADDR = "127.0.0.1:17781"


@dataclass
class AgentSpec:
    """Per-agent metadata for the scenario test."""
    name: str
    binary: str
    # Subdirectory of the agent's state dir (under ~/.<name>/) where
    # transcripts land. Anything new under this path is considered a
    # test artifact and removed at teardown.
    transcript_subdir: str
    skip_on: set[str] = field(default_factory=set)


AGENTS = [
    AgentSpec("claude",  "claude",       "projects"),
    AgentSpec("codex",   "codex",        "sessions"),
    AgentSpec("copilot", "copilot",      "session-state"),
    AgentSpec("cursor",  "cursor-agent", "projects"),
    AgentSpec("gemini",  "gemini",       "tmp"),
    AgentSpec("pi",      "pi",           "agent/sessions"),
]


def find_agent(spec: AgentSpec) -> str | None:
    if shutil.which(spec.binary):
        return spec.binary
    if spec.name == "cursor" and shutil.which("agent"):
        return "agent"
    return None


# -------- mock server / daemon (lifted from agent_matrix_test.py) --------

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

    def send_binary(self, relay_id: str, frame: dict):
        self._http("POST", f"/_admin/sessions/{relay_id}/send_binary", frame)

    def enroll_host(self, device_id: str, session_id: str):
        self._http("POST", "/session/enroll", {
            "device_id": device_id, "session_id": session_id, "hostname": "scenario-host",
        })

    def shutdown(self):
        self.proc.send_signal(signal.SIGTERM)
        try: self.proc.wait(timeout=3)
        except subprocess.TimeoutExpired: self.proc.kill()


class Daemon:
    def __init__(self, bin_path: Path, *, sock: str, home: Path, tmpdir: Path,
                 device_id: str, session_id: str, verbose: bool, log_path: Path | None = None):
        env = {
            "HOME": str(home),
            "PATH": os.environ.get("PATH", ""),
            "TMPDIR": str(tmpdir),
            "GREENLIGHT_DAEMON_SOCK": sock,
            "GREENLIGHT_DEVICE_ID": device_id,
            "GREENLIGHT_DAEMON_SESSION_ID": session_id,
            # Per-test log path so we don't fight the user's prod
            # daemon for ~/.greenlight/daemon.log. Greenlight respects
            # GREENLIGHT_LOG for the daemon process itself.
            "GREENLIGHT_LOG": str(log_path) if log_path else
                              str(tmpdir / "greenlight-daemon-internal.log"),
        }
        # Always capture daemon stderr to a file (even when not verbose)
        # so post-mortem diagnosis is possible — agent scenarios fail in
        # subtle ways (wrong tool, refused prompt) and the daemon log is
        # the first place to look.
        self.log_path = log_path
        log = open(log_path, "w") if log_path else (subprocess.DEVNULL if not verbose else None)
        self.sock = sock
        self.proc = subprocess.Popen(
            [str(bin_path), "daemon", "start", "--foreground"],
            env=env,
            stdout=log if log_path else (None if verbose else subprocess.DEVNULL),
            stderr=log if log_path else (None if verbose else subprocess.DEVNULL),
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


# -------- contamination plumbing --------

def setup_test_home(tmp: Path, spec: AgentSpec) -> Path:
    """Use the user's real HOME. We attempted to sandbox HOME via
    symlinks/copies but agents like claude perform install-integrity
    checks (PATH-resolution of their own binary, login-state lookups)
    that fail when HOME is anything other than the real user dir.
    Cleanup is handled separately: snapshot_transcripts captures
    transcript-dir mtimes before the test and cleanup_transcripts
    removes anything newer afterwards. Daemon state under
    ~/.greenlight/ is cleaned by the per-test cleanup hook below."""
    return Path(os.path.expanduser("~"))


# -------- per-agent trust files --------
#
# Each interactive agent (codex/gemini/copilot/claude) refuses to run
# in a fresh workdir until the user accepts an interactive "trust this
# folder?" prompt. We add the workdir to the agent's trust file before
# launch and revert at teardown so the user's production trust list is
# left exactly as we found it.
#
# cursor/pi don't have folder-trust prompts (cursor gates via login
# state, pi has no equivalent) so trust_workdir is a no-op for them.


def trust_workdir(spec: AgentSpec, workdir: Path) -> dict:
    """Pre-trust `workdir` for the given agent. Returns a snapshot the
    caller passes to restore_trust to revert the change."""
    home = Path(os.path.expanduser("~"))
    snap = {"name": spec.name, "files": {}}
    abs_path = str(workdir.resolve())

    if spec.name == "claude":
        path = home / ".claude.json"
        snap["files"][str(path)] = path.read_bytes() if path.exists() else None
        data = json.loads(path.read_text()) if path.exists() else {}
        projs = data.setdefault("projects", {})
        projs[abs_path] = {**projs.get(abs_path, {}), "hasTrustDialogAccepted": True}
        path.write_text(json.dumps(data, indent=2))

    elif spec.name == "gemini":
        path = home / ".gemini" / "trustedFolders.json"
        snap["files"][str(path)] = path.read_bytes() if path.exists() else None
        data = json.loads(path.read_text()) if path.exists() else {}
        data[abs_path] = "TRUST_FOLDER"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data, indent=2))

    elif spec.name == "codex":
        path = home / ".codex" / "config.toml"
        snap["files"][str(path)] = path.read_bytes() if path.exists() else None
        existing = path.read_text() if path.exists() else ""
        marker = f'[projects."{abs_path}"]'
        if marker not in existing:
            block = f'\n{marker}\ntrust_level = "trusted"\n'
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(existing + block)

    elif spec.name == "copilot":
        path = home / ".copilot" / "config.json"
        snap["files"][str(path)] = path.read_bytes() if path.exists() else None
        # copilot writes its config with leading `// ...` comment lines
        # which standard json.loads chokes on. Strip them before parsing.
        raw = path.read_text() if path.exists() else "{}"
        clean = "\n".join(
            ln for ln in raw.splitlines() if not ln.lstrip().startswith("//")
        )
        data = json.loads(clean) if clean.strip() else {}
        trusted = data.setdefault("trustedFolders", [])
        if abs_path not in trusted:
            trusted.append(abs_path)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data, indent=2))

    return snap


def restore_trust(snap: dict):
    """Revert every trust file the matching trust_workdir call touched
    to its byte-for-byte pre-test state."""
    for path_str, original in snap["files"].items():
        path = Path(path_str)
        if original is None:
            try: path.unlink()
            except OSError: pass
        else:
            try: path.write_bytes(original)
            except OSError: pass


def snapshot_greenlight() -> dict:
    """Snapshot the parts of ~/.greenlight/ the daemon writes during a
    session. Returned shape is enough for restore_greenlight to revert
    the directory to its pre-test state."""
    gl = Path(os.path.expanduser("~/.greenlight"))
    snap = {
        "sessions_json": None,
        "session_files": set(),
    }
    sj = gl / "sessions.json"
    if sj.exists():
        snap["sessions_json"] = sj.read_bytes()
    sd = gl / "sessions"
    if sd.exists():
        snap["session_files"] = {str(p) for p in sd.iterdir()}
    return snap


def restore_greenlight(snap: dict):
    """Revert ~/.greenlight/ to the state captured by snapshot_greenlight.
    Keeps the daemon log and any other files we don't track."""
    gl = Path(os.path.expanduser("~/.greenlight"))
    sj = gl / "sessions.json"
    if snap["sessions_json"] is not None:
        try:
            sj.write_bytes(snap["sessions_json"])
        except OSError:
            pass
    elif sj.exists():
        try:
            sj.unlink()
        except OSError:
            pass
    sd = gl / "sessions"
    if sd.exists():
        for p in sd.iterdir():
            if str(p) not in snap["session_files"]:
                try:
                    p.unlink()
                except OSError:
                    pass


def snapshot_transcripts(spec: AgentSpec) -> set[str]:
    """Record absolute paths of every file under the agent's transcript
    subdir. Used by cleanup_transcripts to delete only files the test
    created."""
    real_dir = Path(os.path.expanduser(f"~/.{spec.name}")) / spec.transcript_subdir
    if not real_dir.exists():
        return set()
    return {str(p) for p in real_dir.rglob("*") if p.is_file()}


def cleanup_transcripts(spec: AgentSpec, before: set[str]):
    """Remove any file under the transcript subdir that wasn't present
    in `before`. Best-effort: missing parent dirs and permission errors
    are swallowed so a partially-broken test never wedges teardown."""
    real_dir = Path(os.path.expanduser(f"~/.{spec.name}")) / spec.transcript_subdir
    if not real_dir.exists():
        return
    for p in real_dir.rglob("*"):
        if not p.is_file():
            continue
        if str(p) in before:
            continue
        try:
            p.unlink()
        except OSError:
            pass


# -------- prompt injection + permission allow loop --------

def inject_input(ms: MockServer, relay_id: str, text: str):
    """Send agent input as a single relay text frame: the production
    server sends whole messages this way (not character-by-character).
    The daemon's handleTextFrame base64-decodes `data` and writes the
    payload directly to the agent's PTY master."""
    try:
        ms.send_text(relay_id, {
            "type": "input",
            "relay_id": relay_id,
            "data": base64.b64encode(text.encode()).decode(),
        })
    except urllib.error.HTTPError as e:
        if e.code != 404:
            raise


def allow_loop(ms: MockServer, relay_id: str, observed: list, stop):
    """Auto-allow every permission_request and append the parsed
    request to `observed` so the test can assert tool/path."""
    handled = set()
    while not stop.get("stop"):
        try:
            inbox = ms.inbox(relay_id)
        except Exception:
            time.sleep(0.1); continue
        for frame in inbox:
            if not isinstance(frame, dict):
                continue
            if frame.get("type") != "permission_request":
                continue
            data = frame.get("data") or {}
            rid = data.get("request_id")
            if not rid or rid in handled:
                continue
            handled.add(rid)
            observed.append({
                "tool": data.get("tool_name", ""),
                "input": data.get("tool_input") or {},
            })
            try:
                ms.send_text(relay_id, {
                    "type": "permission_response",
                    "request_id": rid,
                    "behavior": "allow",
                    "message": "scenario auto-allow",
                })
            except Exception:
                pass
        time.sleep(0.1)


def wait_for_session(ms: MockServer, timeout: float) -> str | None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        for s in ms.sessions():
            return s["relay_id"]
        time.sleep(0.2)
    return None


def wait_for_predicate(predicate, timeout: float, interval: float = 0.5) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return False


def saw_tool_for(observed: list, file_path: str) -> bool:
    """Return True if any observed permission_request mentions the
    given path. Lenient by design — agents pick Write or Bash freely
    and frame paths in different shapes."""
    for r in observed:
        if file_path in json.dumps(r["input"]):
            return True
    return False


# -------- the scenario --------

def go_build(verbose: bool) -> tuple[Path, Path]:
    out = REPO_ROOT / ".build" / "scenario"
    out.mkdir(parents=True, exist_ok=True)
    gl_bin = out / "greenlight"
    ms_bin = out / "mockserver"
    ldflags = (f"-X main.version=0.0.0-scenario "
               f"-X main.wsURL=ws://{MOCK_ADDR}/ws/relay")
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


def run_scenario(spec: AgentSpec, gl_bin: Path, ms: MockServer, tmp: Path,
                 *, verbose: bool, settle: float = 8.0,
                 step_timeout: float = 60.0,
                 keep_transcripts: bool = False) -> tuple[bool, str]:
    if platform.system().lower() in spec.skip_on:
        return True, f"skipped on {platform.system()}"
    if not find_agent(spec):
        return True, "not installed"
    # claude on macOS refuses to authenticate when launched from inside
    # another claude session — it inherits audit-session attributes from
    # the outer claude that fail its self-auth check, returning a
    # synthetic "Not logged in" message. Confirmed Linux is unaffected
    # (audit sessions are macOS-only), so we only skip on darwin. Run
    # this script from a fresh Terminal window on macOS, or use any
    # Linux box, to test claude under either scenario.
    if (spec.name == "claude" and platform.system() == "Darwin"
            and os.environ.get("CLAUDECODE") == "1"):
        return True, ("skipped: cannot run inside another claude session on "
                      "macOS — open a fresh terminal, or run on Linux")

    # Per-test workspace; HOME stays the user's real one (see
    # setup_test_home for why). The workdir lives under
    # ~/greenlight-test-workdir/ (NOT /tmp) — the interpose library
    # auto-allows every write under /tmp without consulting the
    # daemon, so a /tmp workdir would mean the agent's Write tool
    # calls never fire a permission_request and the test can't observe
    # the round-trip. We clean up the per-test subdir explicitly at
    # the end of run_scenario.
    real_home = Path(os.path.expanduser("~"))
    workdir_root = real_home / "greenlight-test-workdir"
    workdir_root.mkdir(parents=True, exist_ok=True)
    workdir = workdir_root / f"scenario-{spec.name}-{uuid.uuid4().hex[:8]}"
    workdir.mkdir(parents=True, exist_ok=True)
    home = setup_test_home(tmp, spec)
    transcript_snapshot = snapshot_transcripts(spec)
    greenlight_snapshot = snapshot_greenlight()
    trust_snapshot = trust_workdir(spec, workdir)

    sock = str(tmp / f"daemon-{spec.name}.sock")
    device_id = f"scenario-{spec.name}"
    session_id = f"00000000-0000-0000-0000-00000000{ord(spec.name[0]):04x}"

    # Unique target the prompt will reference. Use a relative filename
    # (no leading /) so claude's TUI doesn't treat it as a slash-command
    # trigger and divert into autocomplete instead of submitting the
    # message. The agent runs with cwd=workdir so the relative path
    # resolves correctly.
    target_name = f"greenlight-test-{uuid.uuid4().hex[:8]}.txt"
    target = workdir / target_name

    ms.enroll_host(device_id, session_id)
    daemon_log = tmp / f"daemon-{spec.name}.log"
    daemon = Daemon(gl_bin, sock=sock, home=home, tmpdir=tmp, device_id=device_id,
                    session_id=session_id, verbose=verbose, log_path=daemon_log)

    client = None
    master = None
    observed: list = []
    stop = {"stop": False}
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
             "--device-id", device_id, "--project", "scenario",
             "--agent", spec.name],
            cwd=workdir, env=env,
            stdin=slave, stdout=slave, stderr=slave,
            preexec_fn=os.setsid,
        )
        os.close(slave)

        relay_id = wait_for_session(ms, timeout=20.0)
        if not relay_id:
            return False, "session never registered"

        # Drain PTY (and tee into a file for post-mortem diagnosis).
        pty_log = tmp / f"pty-{spec.name}.log"
        threading.Thread(
            target=lambda: _drain_pty(master, settle + 3 * step_timeout, pty_log),
            daemon=True,
        ).start()

        # Allow loop runs until teardown.
        allow_t = threading.Thread(
            target=allow_loop, args=(ms, relay_id, observed, stop), daemon=True,
        ); allow_t.start()

        # Let the agent finish booting before we type at it.
        time.sleep(settle)
        if client.poll() is not None:
            return False, f"agent exited during boot (code={client.returncode})"

        # Step 1: ask for a file to be created. Keep prompt short and
        # imperative — chatty/qualified prompts let the agent wander
        # into WebFetch or asking-clarifying-questions territory.
        # Use a relative filename so the leading "/" of an absolute
        # path doesn't trip claude's slash-command autocomplete.
        prompt1 = f"Create a file named {target_name} in the current directory containing the text hi.\r"
        inject_input(ms, relay_id, prompt1)
        if not wait_for_predicate(lambda: target.exists(), step_timeout):
            return False, (f"file never created at {target}; "
                           f"observed={[r['tool'] for r in observed]}")
        if not saw_tool_for(observed, str(target)):
            return False, ("file created but no permission_request referenced it; "
                           f"observed={observed}")

        creates = list(observed)

        # Step 2: ask for the file to be deleted.
        prompt2 = f"Delete the file {target_name}.\r"
        inject_input(ms, relay_id, prompt2)
        if not wait_for_predicate(lambda: not target.exists(), step_timeout):
            return False, (f"file never deleted at {target}; "
                           f"observed_after_create={[r['tool'] for r in observed[len(creates):]]}")
        if not saw_tool_for(observed[len(creates):], str(target)):
            return False, "file deleted but no later permission_request referenced it"

        # Step 3: kill via control frame, expect clean exit.
        ms.send_binary(relay_id, {"type": "kill", "relay_id": relay_id})
        if not wait_for_predicate(lambda: client.poll() is not None, 10.0):
            return False, "agent did not exit after kill control frame"

        return True, (f"create={len(creates)} delete={len(observed) - len(creates)} "
                      f"tools={sorted({r['tool'] for r in observed})}")
    finally:
        stop["stop"] = True
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
        if not keep_transcripts:
            cleanup_transcripts(spec, transcript_snapshot)
        restore_greenlight(greenlight_snapshot)
        restore_trust(trust_snapshot)
        # Workdir lives under the user's HOME (~/greenlight-test-workdir/)
        # rather than /tmp so the agent's writes get gated. Clean it up
        # explicitly since shutil.rmtree(tmp) won't reach it.
        try:
            shutil.rmtree(workdir, ignore_errors=True)
        except OSError:
            pass
        # Surface the daemon log when the test failed so the user can
        # see what tools the agent actually invoked.
        if observed and not all(saw_tool_for([r], str(target)) for r in observed):
            log_text = ""
            if daemon.log_path and daemon.log_path.exists():
                log_text = daemon.log_path.read_text()
            if log_text and verbose:
                print("--- daemon log ---")
                print(log_text[-4000:])  # tail


def _drain_pty(master: int, total_timeout: float, sink_path: Path | None = None):
    import select
    sink = open(sink_path, "wb") if sink_path else None
    try:
        deadline = time.time() + total_timeout
        while time.time() < deadline:
            rfds, _, _ = select.select([master], [], [], 0.5)
            if master not in rfds:
                continue
            try:
                chunk = os.read(master, 4096)
            except OSError:
                return
            if not chunk:
                return
            if sink:
                sink.write(chunk); sink.flush()
    finally:
        if sink:
            sink.close()


# -------- main --------

def main():
    p = argparse.ArgumentParser()
    p.add_argument("--only", nargs="+", help="restrict to these agents")
    p.add_argument("-v", "--verbose", action="store_true")
    p.add_argument("--keep", action="store_true",
                   help="don't tear down temp dir on exit")
    p.add_argument("--keep-transcripts", action="store_true",
                   help="don't restore the agent's transcript dir on teardown — useful for diagnosing why an agent didn't engage with the prompt")
    p.add_argument("--settle", type=float, default=8.0,
                   help="seconds to let each agent boot before prompting")
    p.add_argument("--step-timeout", type=float, default=60.0,
                   help="per-step timeout (file creation, deletion)")
    args = p.parse_args()

    print("==> building greenlight + mockserver")
    gl_bin, ms_bin = go_build(args.verbose)

    selected = AGENTS
    if args.only:
        selected = [a for a in AGENTS if a.name in args.only]
        if not selected:
            print(f"no matching agents: {args.only}", file=sys.stderr)
            return 2

    tmp = Path(tempfile.mkdtemp(prefix="gl-scenario-"))
    print(f"workspace: {tmp}")

    ms = MockServer(ms_bin, MOCK_ADDR, args.verbose)
    results = []
    try:
        for spec in selected:
            print(f"\n==> {spec.name}")
            t0 = time.time()
            try:
                ok, reason = run_scenario(spec, gl_bin, ms, tmp,
                                          verbose=args.verbose,
                                          settle=args.settle,
                                          step_timeout=args.step_timeout,
                                          keep_transcripts=args.keep_transcripts)
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
