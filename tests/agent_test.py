#!/usr/bin/env python3
"""
Greenlight Interpose Test Harness
=================================

Tests that the interpose library correctly intercepts tool calls made by
different agent runtimes. No server, no session enrollment — just the
interpose library, a local Unix socket that auto-allows everything, and
pexpect to drive the agents.

For each agent, the harness:
  1. Creates a Unix socket that auto-allows all requests and logs them
  2. Spawns the agent with DYLD_INSERT_LIBRARIES/LD_PRELOAD + socket env
  3. Sends prompts that trigger specific tool types
  4. Checks that the expected interpose requests arrived at the socket

Requirements:
    pip install pexpect

Usage:
    # Test all agents (auto-extract lib from greenlight binary)
    python tests/agent_test.py

    # Test specific agents
    python tests/agent_test.py --agents claude codex

    # Provide interpose library path directly
    python tests/agent_test.py --lib /path/to/libgreenlight.dylib

    # Use a specific greenlight binary for lib extraction
    python tests/agent_test.py --greenlight ./greenlight-dev

    # Run specific scenarios only
    python tests/agent_test.py --scenarios bash file_read

    # Verbose mode (show agent terminal output)
    python tests/agent_test.py -v

    # Dry run
    python tests/agent_test.py --dry-run

Environment variables:
    GREENLIGHT_BINARY  - Path to greenlight binary (for lib extraction)
    GREENLIGHT_LIB     - Path to interpose library (skips extraction)

Notes:
    - Tests run in ~/greenlight-test-workdir (not /tmp, since the interpose
      C library auto-allows all file ops under /tmp and $TMPDIR).
    - On macOS, agent binaries are re-signed for DYLD entitlement if needed.
    - On Linux, a seccomp USER_NOTIF supervisor auto-allows write syscalls.

Known limitations:
    - Copilot (macOS): copilot's Node.js loader re-execs itself via
      child_process.spawn with a custom env. macOS dyld strips
      DYLD_INSERT_LIBRARIES from process.env after loading, so the child
      process doesn't inherit it. This is a real interpose propagation bug.
    - Codex file_read: codex exec mode struggles with file read prompts
      (LLM behavior, not an interpose issue).
    - Codex on Linux: the Rust vendor binary is musl-linked, so LD_PRELOAD
      has no effect. Needs seccomp supervisor support.
"""

import argparse
import json
import os
import platform
import random
import signal
import socket
import string
import struct
import sys
import tempfile
import threading
import time
from dataclasses import dataclass, field
from enum import Enum
from typing import Optional

try:
    import pexpect
except ImportError:
    print("Error: pexpect is required. Install with: pip install pexpect", file=sys.stderr)
    sys.exit(1)


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

ALL_AGENTS = ["claude", "codex", "copilot", "cursor", "gemini", "pi"]

# Agent binary names
AGENT_BINARIES = {
    "claude": "claude",
    "codex": "codex",
    "copilot": "copilot",
    "cursor": "agent",
    "gemini": "gemini",
    "pi": "pi",
}

# Flags to bypass each agent's built-in permission prompts and enable
# non-interactive (print) mode. We spawn one process per scenario.
# The prompt is appended as the final argument.
AGENT_BYPASS_FLAGS = {
    "claude": ["--dangerously-skip-permissions", "-p"],
    "codex": ["--dangerously-bypass-approvals-and-sandbox", "exec"],
    "copilot": ["--allow-all", "-p"],
    "cursor": ["--yolo"],  # cursor has no print mode — interactive only
    "gemini": ["-p"],
    "pi": ["--dangerously-skip-permissions", "-p"],
}

# Agents that use print/non-interactive mode: one spawn per scenario.
# All agents with a -p/exec mode should be listed here.
PRINT_MODE_AGENTS = {"claude", "codex", "copilot", "gemini", "pi"}

# How long to wait for the agent to become ready (seconds)
DEFAULT_READY_TIMEOUT = 60

# How long to wait after sending a prompt for interpose events (seconds)
# In print mode, agents need time to think + execute tool calls.
SCENARIO_TIMEOUT = 120

# How long to wait after sending exit signal (seconds)
EXIT_TIMEOUT = 10

IS_DARWIN = platform.system() == "Darwin"
IS_LINUX = platform.system() == "Linux"


# ---------------------------------------------------------------------------
# macOS DYLD entitlement handling
# ---------------------------------------------------------------------------

def resolve_script_command(binary_path: str, args: list[str]) -> tuple[str, list[str]]:
    """If binary_path is a script with a shebang, resolve to the interpreter.

    On macOS, /usr/bin/env is SIP-protected and strips DYLD_INSERT_LIBRARIES.
    We detect shebang scripts and launch the interpreter directly, same as
    greenlight's resolveScriptCommand.
    """
    if not IS_DARWIN:
        return binary_path, args

    import shutil

    try:
        with open(binary_path, "rb") as f:
            header = f.read(256)
    except (OSError, IOError):
        return binary_path, args

    if not header.startswith(b"#!"):
        return binary_path, args  # Not a script

    # Parse shebang line
    shebang = header.split(b"\n")[0][2:].decode("utf-8", errors="replace").strip()
    parts = shebang.split()
    if not parts:
        return binary_path, args

    # Handle /usr/bin/env interpreter
    if parts[0] == "/usr/bin/env":
        # Skip env flags like -S
        interp_parts = [p for p in parts[1:] if not p.startswith("-")]
        if interp_parts:
            interp_name = interp_parts[0]
            resolved_interp = shutil.which(interp_name)
            if resolved_interp:
                # Launch: interpreter [script_path] [original_args...]
                new_args = [binary_path] + args
                print(f"  Resolved shebang: {resolved_interp} {binary_path}",
                      file=sys.stderr, flush=True)
                return resolved_interp, new_args
    elif os.path.isfile(parts[0]):
        # Direct interpreter path
        new_args = parts[1:] + [binary_path] + args
        return parts[0], new_args

    return binary_path, args


def ensure_dyld_entitlement(binary_path: str) -> bool:
    """On macOS, ensure the binary has allow-dyld-environment-variables.
    Re-signs if needed (same as greenlight's ensureDyldEntitlement).
    Returns True if entitlement is present (or was added)."""
    if not IS_DARWIN:
        return True

    import subprocess
    import shutil

    resolved = shutil.which(binary_path) or binary_path
    if not os.path.isfile(resolved):
        return False

    # Check current entitlements
    try:
        out = subprocess.check_output(
            ["codesign", "-d", "--entitlements", "-", "--xml", resolved],
            stderr=subprocess.DEVNULL,
        )
        if b"allow-dyld-environment-variables" in out:
            return True  # already has it
    except subprocess.CalledProcessError:
        pass  # not signed

    print(f"  Re-signing {resolved} to add dyld entitlement...", file=sys.stderr)

    plist = """<?xml version="1.0" encoding="UTF-8"?>
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
</plist>"""

    plist_path = os.path.join(tempfile.gettempdir(), "gl-test-entitlements.plist")
    with open(plist_path, "w") as f:
        f.write(plist)

    try:
        # Two-step: remove old sig, then sign fresh (preserves SEA binaries)
        subprocess.run(["codesign", "--remove-signature", resolved],
                       check=True, capture_output=True)
        subprocess.run(["codesign", "--sign", "-", "--entitlements", plist_path,
                         "--options", "runtime", resolved],
                       check=True, capture_output=True)
        print(f"  Re-signed {resolved} successfully", file=sys.stderr)
        return True
    except subprocess.CalledProcessError as e:
        print(f"  Failed to re-sign {resolved}: {e}", file=sys.stderr)
        return False
    finally:
        os.unlink(plist_path)


# ---------------------------------------------------------------------------
# Interpose socket handler (auto-allow + logging)
# ---------------------------------------------------------------------------

class InterposeHandler:
    """Unix socket that mimics greenlight's interpose handler.
    Auto-allows everything and records all requests."""

    def __init__(self, sock_path: str, verbose: bool = False):
        self.sock_path = sock_path
        self.verbose = verbose
        self.requests: list[dict] = []
        self._lock = threading.Lock()
        self._server: Optional[socket.socket] = None
        self._thread: Optional[threading.Thread] = None
        self._running = False

    def start(self):
        self._server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._server.bind(self.sock_path)
        self._server.listen(32)
        self._server.settimeout(1.0)
        self._running = True
        self._thread = threading.Thread(target=self._accept_loop, daemon=True)
        self._thread.start()

    def stop(self):
        self._running = False
        if self._server:
            self._server.close()
        if self._thread:
            self._thread.join(timeout=5)
        try:
            os.unlink(self.sock_path)
        except OSError:
            pass

    def get_requests(self) -> list[dict]:
        with self._lock:
            return list(self.requests)

    def clear_requests(self):
        with self._lock:
            self.requests.clear()

    def _accept_loop(self):
        while self._running:
            try:
                conn, _ = self._server.accept()
                threading.Thread(
                    target=self._handle_conn, args=(conn,), daemon=True
                ).start()
            except socket.timeout:
                continue
            except OSError:
                break

    def _handle_conn(self, conn: socket.socket):
        try:
            # The interpose library may send ancillary data (SCM_RIGHTS for
            # seccomp fd on Linux). Use recvmsg to handle both cases.
            seccomp_fd = -1
            try:
                data, ancdata, flags, addr = conn.recvmsg(65536, 4096)
                # Extract file descriptor from ancillary data (SCM_RIGHTS)
                for cmsg_level, cmsg_type, cmsg_data in ancdata:
                    if cmsg_level == socket.SOL_SOCKET and cmsg_type == socket.SCM_RIGHTS:
                        seccomp_fd = struct.unpack("i", cmsg_data[:4])[0]
            except (AttributeError, OSError):
                # Fallback for systems where recvmsg isn't available
                data = conn.recv(65536)

            if not data:
                return

            line = data.strip()
            if not line:
                return

            try:
                req = json.loads(line)
            except json.JSONDecodeError:
                return

            # Handle seccomp fd handoff (Linux): start a supervisor thread
            # that auto-allows all seccomp notifications.
            if req.get("type") == "seccomp_fd":
                if seccomp_fd >= 0:
                    if self.verbose:
                        print(f"  [interpose] seccomp fd received: {seccomp_fd}, "
                              f"starting auto-allow supervisor", file=sys.stderr, flush=True)
                    threading.Thread(
                        target=self._seccomp_supervisor, args=(seccomp_fd,),
                        daemon=True
                    ).start()
                return

            if self.verbose:
                pid = req.get('pid', '?')
                print(f"  [interpose] pid={pid} {req.get('type', '?')}: "
                      f"{self._summarize(req)}", file=sys.stderr, flush=True)

            with self._lock:
                self.requests.append(req)

            # Auto-allow
            response = json.dumps({"allow": True}) + "\n"
            conn.sendall(response.encode())
        except Exception as e:
            if self.verbose:
                print(f"  [interpose] handler error: {e}", file=sys.stderr)
        finally:
            conn.close()

    def _seccomp_supervisor(self, notif_fd: int):
        """Auto-allow all seccomp USER_NOTIF notifications.

        On Linux, the interpose library installs a seccomp filter with
        SECCOMP_RET_USER_NOTIF for write-mode opens and renames. It sends
        the notification fd to us via SCM_RIGHTS. We must respond to each
        notification or the target process hangs.
        """
        import ctypes
        import ctypes.util

        libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)

        # ioctl constants for seccomp notifications
        # SECCOMP_IOCTL_NOTIF_RECV = SECCOMP_IOC_MAGIC 'R' (0xc0502100)
        # SECCOMP_IOCTL_NOTIF_SEND = SECCOMP_IOC_MAGIC 'S' (0xc0182101)
        # These vary by arch — use the values for x86_64/aarch64
        SECCOMP_IOCTL_NOTIF_RECV = 0xc0502100
        SECCOMP_IOCTL_NOTIF_SEND = 0xc0182101

        # struct seccomp_notif (recv) — 80 bytes on x86_64
        # { __u64 id, __u32 pid, __u32 flags, struct seccomp_data data }
        # seccomp_data is 64 bytes: { int nr, __u32 arch, __u64 ip, __u64 args[6] }
        NOTIF_SIZE = 80

        # struct seccomp_notif_resp (send) — 24 bytes
        # { __u64 id, __s64 val, __u32 error, __u32 flags }
        RESP_SIZE = 24
        SECCOMP_USER_NOTIF_FLAG_CONTINUE = 0x00000001

        notif_buf = ctypes.create_string_buffer(NOTIF_SIZE)
        resp_buf = ctypes.create_string_buffer(RESP_SIZE)

        while self._running:
            # Zero the buffer
            ctypes.memset(notif_buf, 0, NOTIF_SIZE)

            ret = libc.ioctl(notif_fd, SECCOMP_IOCTL_NOTIF_RECV, notif_buf)
            if ret != 0:
                errno_val = ctypes.get_errno()
                if errno_val == 4:  # EINTR
                    continue
                if self.verbose:
                    print(f"  [seccomp] recv failed: errno={errno_val}",
                          file=sys.stderr, flush=True)
                break

            # Extract notification ID (first 8 bytes, __u64)
            notif_id = struct.unpack("Q", notif_buf[:8])[0]

            # Build response: allow with CONTINUE flag
            struct.pack_into("Q", resp_buf, 0, notif_id)    # id
            struct.pack_into("q", resp_buf, 8, 0)           # val (0 = success)
            struct.pack_into("I", resp_buf, 16, 0)          # error (0)
            struct.pack_into("I", resp_buf, 20, SECCOMP_USER_NOTIF_FLAG_CONTINUE)  # flags

            ret = libc.ioctl(notif_fd, SECCOMP_IOCTL_NOTIF_SEND, resp_buf)
            if ret != 0:
                errno_val = ctypes.get_errno()
                if errno_val == 2:  # ENOENT — process died
                    continue
                if self.verbose:
                    print(f"  [seccomp] send failed: errno={errno_val}",
                          file=sys.stderr, flush=True)

        os.close(notif_fd)

    def _summarize(self, req: dict) -> str:
        t = req.get("type", "")
        if t == "spawn":
            args = req.get("args", [])
            return " ".join(args[:3]) + ("..." if len(args) > 3 else "")
        elif t in ("open", "read"):
            return req.get("path", "")
        elif t == "connect":
            return f"{req.get('host', '')}:{req.get('port', '')}"
        return str(req)


# ---------------------------------------------------------------------------
# Interpose library extraction
# ---------------------------------------------------------------------------

def find_interpose_lib(lib_path: str = None, greenlight_bin: str = None) -> str:
    """Find or extract the interpose library.

    Priority:
      1. Explicit --lib path
      2. GREENLIGHT_LIB env var
      3. Extract from greenlight binary using a temp connect that we kill
      4. Search common locations
    """
    # Explicit path
    if lib_path:
        if os.path.isfile(lib_path):
            return lib_path
        print(f"Error: interpose library not found at {lib_path}", file=sys.stderr)
        sys.exit(1)

    # Env var
    env_lib = os.environ.get("GREENLIGHT_LIB")
    if env_lib and os.path.isfile(env_lib):
        return env_lib

    # Search common temp locations (greenlight extracts to /tmp/.gl-*)
    ext = ".dylib" if IS_DARWIN else ".so"
    for f in os.listdir(tempfile.gettempdir()):
        if f.startswith(".gl-"):
            candidate = os.path.join(tempfile.gettempdir(), f)
            if os.path.isfile(candidate):
                return candidate

    # Try to extract using greenlight binary
    gl_bin = greenlight_bin or os.environ.get("GREENLIGHT_BINARY", "greenlight")
    import shutil
    resolved = shutil.which(gl_bin)
    if not resolved:
        print(f"Error: Cannot find interpose library. Provide --lib or ensure "
              f"greenlight is on PATH.", file=sys.stderr)
        print(f"  The interpose library ({ext}) can be found in greenlight's "
              f"build output or extracted at runtime.", file=sys.stderr)
        sys.exit(1)

    # Extract by running greenlight with a fake device ID — it will extract
    # the lib before failing on enrollment. We just need the temp file.
    print(f"Extracting interpose library from {resolved}...", file=sys.stderr)
    import subprocess
    # Run greenlight connect briefly to trigger lib extraction, then kill it
    proc = subprocess.Popen(
        [resolved, "connect", "--device-id", "00000000-0000-0000-0000-000000000000"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        env={**os.environ, "GREENLIGHT_AGENT": "claude"},
    )
    # Give it a moment to extract, then kill
    time.sleep(2)
    proc.kill()
    proc.wait()

    # Check for extracted lib again
    for f in os.listdir(tempfile.gettempdir()):
        if f.startswith(".gl-"):
            candidate = os.path.join(tempfile.gettempdir(), f)
            if os.path.isfile(candidate):
                return candidate

    print("Error: Could not extract interpose library.", file=sys.stderr)
    sys.exit(1)


# ---------------------------------------------------------------------------
# Test scenarios
# ---------------------------------------------------------------------------

class ToolType(Enum):
    FILE_READ = "file_read"
    FILE_WRITE = "file_write"
    BASH = "bash"
    WEB_FETCH = "web_fetch"


@dataclass
class TestScenario:
    name: str
    tool_type: ToolType
    prompt: str
    # Function that checks the interpose requests for expected events.
    # Returns (passed: bool, detail: str).
    check: object  # Callable[[list[dict]], tuple[bool, str]]
    # Optional setup function called with (cwd) before the scenario runs.
    setup: object = None  # Callable[[str], None]


def _create_file(cwd: str, name: str, content: str):
    """Create a file in the working directory for test setup."""
    path = os.path.join(cwd, name)
    with open(path, "w") as f:
        f.write(content)


def random_suffix():
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=8))


# Interpose requests from agent startup (git, ssh, etc.) that should be
# ignored when checking for tool-triggered events.
STARTUP_NOISE = {
    "spawn": {"git", "ssh", "/usr/bin/git", "/usr/bin/ssh", "git-remote-https"},
    "connect": {"github.com", "api.anthropic.com", "statsig.anthropic.com",
                "sentry.io", "*.anthropic.com"},
}


def _is_noise(req: dict) -> bool:
    """Return True if this request is likely startup noise, not from a tool call."""
    t = req.get("type", "")
    if t == "spawn":
        args = req.get("args", [])
        if args:
            binary = os.path.basename(args[0])
            if binary in STARTUP_NOISE["spawn"]:
                return True
            # Git plumbing
            if binary.startswith("git-"):
                return True
            # Agent binary self-spawns (node wrapper → vendor binary)
            path = args[0].lower()
            for agent_name in ("codex", "copilot", "gemini", "cursor", "claude"):
                if agent_name in path:
                    return True
    if t == "read":
        path = req.get("path", "")
        # Keychain reads are startup noise
        if "Keychains" in path or "keychain" in path:
            return True
    if t == "connect":
        host = req.get("host", "")
        for pattern in STARTUP_NOISE["connect"]:
            if pattern.startswith("*"):
                if host.endswith(pattern[1:]):
                    return True
            elif host == pattern:
                return True
    return False


def check_file_read(requests: list[dict]) -> tuple[bool, str]:
    """Check that a file read was intercepted (excluding agent-internal paths)."""
    for req in requests:
        path = req.get("path", "")
        # Skip agent-internal paths and system paths
        if "/.claude/" in path or "/.local/" in path:
            continue
        if "Keychains" in path or "keychain" in path:
            continue
        if path.startswith("/lib/") or path.startswith("/proc/"):
            continue
        if req.get("type") == "read":
            return True, f"read {path}"
        if req.get("type") == "open" and "w" not in req.get("flags", ""):
            return True, f"open(r) {path}"
    return False, "no file read intercepted"


def check_file_write(target_path: str):
    """Return a checker for file write interception."""
    def check(requests: list[dict]) -> tuple[bool, str]:
        for req in requests:
            path = req.get("path", "")
            if req.get("type") == "open" and "w" in req.get("flags", ""):
                if target_path in path:
                    return True, f"open(w) {path}"
            if req.get("type") == "open" and req.get("flags") == "rename":
                if target_path in path:
                    return True, f"rename {path}"
        # Broader: any non-agent-internal write
        for req in requests:
            path = req.get("path", "")
            if "/.claude/" in path or "/.local/" in path:
                continue
            if req.get("type") == "open" and "w" in req.get("flags", ""):
                return True, f"open(w) {path}"
        # Some agents (codex, pi) write files via shell commands, which
        # show up as spawns containing the target filename or a redirect.
        for req in requests:
            if req.get("type") == "spawn" and not _is_noise(req):
                args = req.get("args", [])
                cmd = " ".join(args)
                if target_path in cmd or "apply_patch" in cmd.lower():
                    return True, f"spawn(write) {cmd[:80]}"
        return False, "no file write intercepted"
    return check


def check_bash(requests: list[dict]) -> tuple[bool, str]:
    """Check that a non-startup command spawn was intercepted."""
    for req in requests:
        if req.get("type") == "spawn" and not _is_noise(req):
            args = req.get("args", [])
            cmd = " ".join(args[:3])
            return True, f"spawn {cmd}"
    return False, "no non-startup spawn intercepted"


def check_web_fetch(requests: list[dict]) -> tuple[bool, str]:
    """Check that a non-startup network connection was intercepted."""
    for req in requests:
        if req.get("type") == "connect" and not _is_noise(req):
            host = req.get("host", "")
            return True, f"connect {host}"
    # Some agents (codex, pi) fetch URLs via shell commands (curl, wget),
    # which show up as spawns containing the URL.
    for req in requests:
        if req.get("type") == "spawn" and not _is_noise(req):
            cmd = " ".join(req.get("args", []))
            if "example.com" in cmd and ("curl" in cmd or "wget" in cmd or "fetch" in cmd):
                return True, f"spawn(fetch) {cmd[:80]}"
    return False, "no web fetch intercepted"


def build_scenarios() -> list[TestScenario]:
    suffix = random_suffix()
    # Target files in the project directory (CWD) — the interpose C library
    # only gates file operations within the project directory. Writes to /tmp
    # and system paths are auto-allowed without hitting the socket.
    write_file = f"gl-test-{suffix}.txt"
    read_file = f"gl-read-{suffix}.txt"
    return [
        TestScenario(
            name="file_read",
            tool_type=ToolType.FILE_READ,
            # We pre-create a file in the project dir for the agent to read
            prompt=f"Read the file ./{read_file} and print its contents.",
            check=check_file_read,
            setup=lambda cwd: _create_file(cwd, read_file, "test content for reading\n"),
        ),
        TestScenario(
            name="file_write",
            tool_type=ToolType.FILE_WRITE,
            prompt=f"Create the file ./{write_file} containing the single word hello.",
            check=check_file_write(write_file),
        ),
        TestScenario(
            name="bash_command",
            tool_type=ToolType.BASH,
            prompt="Run this exact shell command: uname -a",
            check=check_bash,
        ),
        TestScenario(
            name="web_fetch",
            tool_type=ToolType.WEB_FETCH,
            prompt="Fetch the URL https://example.com and show me the HTML.",
            check=check_web_fetch,
        ),
    ]


# ---------------------------------------------------------------------------
# Agent readiness patterns
# ---------------------------------------------------------------------------

# Patterns indicating the agent is ready for user input (after any startup prompts).
# For Claude, we first dismiss the trust prompt, then wait for the input box.
READY_PATTERNS = {
    "claude": [r"╭", r"tips"],
    "codex": [r">", r"╭", r"What would you like", r"\$"],
    "copilot": [r">", r"╭", r"What can I help", r"\$"],
    "cursor": [r">", r"╭", r"What would you like", r"\$"],
    "gemini": [r">", r"╭", r"❯", r"\$"],
}

# Some agents show a trust/safety prompt on first launch in a new directory.
# These patterns trigger sending Enter to dismiss the prompt.
TRUST_PROMPT_PATTERNS = {
    "claude": [r"Yes, I trust", r"trust this folder"],
}


# ---------------------------------------------------------------------------
# Result tracking
# ---------------------------------------------------------------------------

class TestResult(Enum):
    PASS = "PASS"
    FAIL = "FAIL"
    SKIP = "SKIP"
    ERROR = "ERROR"
    TIMEOUT = "TIMEOUT"


@dataclass
class ScenarioResult:
    agent: str
    scenario: str
    tool_type: str
    result: TestResult
    detail: str = ""
    duration: float = 0.0


# ---------------------------------------------------------------------------
# Core test runner
# ---------------------------------------------------------------------------

class AgentTestRunner:
    def __init__(self, lib_path: str, ready_timeout: int, verbose: bool):
        self.lib_path = lib_path
        self.ready_timeout = ready_timeout
        self.verbose = verbose
        self.results: list[ScenarioResult] = []

    def run_agent(self, agent: str, scenarios: list[TestScenario]) -> list[ScenarioResult]:
        results = []

        if agent in PRINT_MODE_AGENTS:
            # Print-mode agents: one spawn per scenario, prompt passed as arg
            results = self._run_print_mode(agent, scenarios)
        else:
            # Interactive agents: single session, send prompts one by one
            results = self._run_interactive(agent, scenarios)

        self.results.extend(results)
        return results

    def _run_print_mode(self, agent: str, scenarios: list[TestScenario]) -> list[ScenarioResult]:
        """Run scenarios using print mode (-p): one process per scenario."""
        results = []

        for scenario in scenarios:
            # Use a non-temp working directory. The interpose C library
            # auto-allows all file ops under /tmp and $TMPDIR, so we need
            # a "real" project directory for file read/write gating to work.
            workdir = os.path.join(os.path.expanduser("~"), "greenlight-test-workdir")
            os.makedirs(workdir, exist_ok=True)

            # Socket must still be in /tmp (short path for Unix socket limit)
            sock_dir = tempfile.mkdtemp(prefix=f"gl-sock-{agent}-")
            sock_path = os.path.join(sock_dir, "gl.sock")
            handler = InterposeHandler(sock_path, verbose=self.verbose)
            handler.start()

            try:
                result = self._run_print_scenario(agent, scenario, workdir, sock_path, handler)
                results.append(result)
            finally:
                handler.stop()
                # Clean up socket dir but leave workdir for inspection
                import shutil
                shutil.rmtree(sock_dir, ignore_errors=True)

        return results

    def _run_print_scenario(self, agent: str, scenario: TestScenario,
                            cwd: str, sock_path: str,
                            handler: InterposeHandler) -> ScenarioResult:
        """Spawn agent in print mode with the prompt, monitor socket."""
        start = time.time()
        self._log(f"  [{agent}] {scenario.name}: {scenario.prompt[:60]}...")

        # Run optional setup (e.g. create files for read tests)
        if scenario.setup:
            scenario.setup(cwd)

        binary = AGENT_BINARIES[agent]
        args = list(AGENT_BYPASS_FLAGS.get(agent, []))
        args.append(scenario.prompt)

        import shutil
        resolved = shutil.which(binary)
        if not resolved:
            return ScenarioResult(
                agent=agent, scenario=scenario.name, tool_type=scenario.tool_type.value,
                result=TestResult.ERROR, detail=f"Binary '{binary}' not found",
            )

        # Resolve shebang scripts (macOS: /usr/bin/env strips DYLD)
        resolved, args = resolve_script_command(resolved, args)

        if IS_DARWIN and not ensure_dyld_entitlement(resolved):
            return ScenarioResult(
                agent=agent, scenario=scenario.name, tool_type=scenario.tool_type.value,
                result=TestResult.ERROR, detail="Cannot ensure dyld entitlement",
            )

        env = os.environ.copy()
        if IS_DARWIN:
            env["DYLD_INSERT_LIBRARIES"] = self.lib_path
        elif IS_LINUX:
            env["LD_PRELOAD"] = self.lib_path
        env["GREENLIGHT_INTERPOSE_SOCK"] = sock_path
        env["GREENLIGHT_AGENT"] = agent
        env["GREENLIGHT_INTERPOSE_LOG"] = os.path.join(cwd, "interpose.log")
        # These env vars tell the interpose C library that greenlight is
        # actively managing the session — without them, file reads/writes
        # are auto-allowed (same as hook passthrough behavior).
        env["GREENLIGHT_DEVICE_ID"] = "00000000-0000-0000-0000-000000000000"
        env["GREENLIGHT_SESSION_ID"] = f"test-{random_suffix()}"
        env["GREENLIGHT_PROJECT"] = "gl-test"

        self._log(f"    Spawning: {resolved} {' '.join(args[:3])}...")

        try:
            child = pexpect.spawn(
                resolved, args,
                cwd=cwd, env=env,
                encoding="utf-8",
                timeout=SCENARIO_TIMEOUT,
                maxread=65536,
                dimensions=(40, 120),
            )
            if self.verbose:
                child.logfile_read = sys.stdout

            # Wait for process to finish or timeout, polling the socket
            deadline = time.time() + SCENARIO_TIMEOUT
            while time.time() < deadline:
                if not child.isalive():
                    # Process finished — give socket handler a moment to process
                    time.sleep(0.5)
                    break
                time.sleep(1)
                # Check early if we already got what we need
                requests = handler.get_requests()
                passed, detail = scenario.check(requests)
                if passed:
                    duration = time.time() - start
                    self._log(f"    PASS ({duration:.1f}s): {detail}")
                    self._cleanup(child)
                    return ScenarioResult(
                        agent=agent, scenario=scenario.name,
                        tool_type=scenario.tool_type.value,
                        result=TestResult.PASS,
                        detail=detail, duration=duration,
                    )

            # Final check
            requests = handler.get_requests()
            passed, detail = scenario.check(requests)
            duration = time.time() - start

            if passed:
                self._log(f"    PASS ({duration:.1f}s): {detail}")
                return ScenarioResult(
                    agent=agent, scenario=scenario.name,
                    tool_type=scenario.tool_type.value,
                    result=TestResult.PASS, detail=detail, duration=duration,
                )

            types_seen = [r.get("type", "?") for r in requests]
            self._log(f"    FAIL ({duration:.1f}s): {detail}")
            if types_seen:
                self._log(f"    Intercepted: {types_seen}")
            else:
                self._log(f"    No interpose requests received")
                # Check interpose log for clues
                log_path = os.path.join(cwd, "interpose.log")
                if os.path.isfile(log_path):
                    with open(log_path) as f:
                        log_content = f.read()
                    if log_content:
                        self._log(f"    Interpose log:\n{log_content[:500]}")

            self._cleanup(child)
            return ScenarioResult(
                agent=agent, scenario=scenario.name,
                tool_type=scenario.tool_type.value,
                result=TestResult.FAIL, detail=detail, duration=duration,
            )

        except Exception as e:
            return ScenarioResult(
                agent=agent, scenario=scenario.name,
                tool_type=scenario.tool_type.value,
                result=TestResult.ERROR, detail=str(e),
                duration=time.time() - start,
            )

    def _run_interactive(self, agent: str, scenarios: list[TestScenario]) -> list[ScenarioResult]:
        """Run scenarios in an interactive session (non-print-mode agents)."""
        results = []

        # Use a non-temp working directory (interpose auto-allows /tmp/$TMPDIR)
        workdir = os.path.join(os.path.expanduser("~"), "greenlight-test-workdir")
        os.makedirs(workdir, exist_ok=True)
        sock_dir = tempfile.mkdtemp(prefix=f"gl-sock-{agent}-")
        sock_path = os.path.join(sock_dir, "gl.sock")
        handler = InterposeHandler(sock_path, verbose=self.verbose)
        handler.start()

        try:
            child = self._spawn_agent(agent, workdir, sock_path)
            if child is None:
                for s in scenarios:
                    results.append(ScenarioResult(
                        agent=agent, scenario=s.name, tool_type=s.tool_type.value,
                        result=TestResult.ERROR, detail="Failed to spawn agent",
                    ))
                return results

            if not self._wait_ready(child, agent):
                self._cleanup(child)
                for s in scenarios:
                    results.append(ScenarioResult(
                        agent=agent, scenario=s.name, tool_type=s.tool_type.value,
                        result=TestResult.TIMEOUT,
                        detail=f"Agent not ready within {self.ready_timeout}s",
                    ))
                return results

            for scenario in scenarios:
                result = self._run_scenario(child, agent, scenario, handler)
                results.append(result)
                time.sleep(2)

                if not child.isalive():
                    idx = scenarios.index(scenario)
                    for remaining in scenarios[idx + 1:]:
                        results.append(ScenarioResult(
                            agent=agent, scenario=remaining.name,
                            tool_type=remaining.tool_type.value,
                            result=TestResult.ERROR,
                            detail="Agent process exited unexpectedly",
                        ))
                    break

            self._cleanup(child)
        finally:
            handler.stop()
            import shutil
            shutil.rmtree(sock_dir, ignore_errors=True)

        return results

    def _spawn_agent(self, agent: str, cwd: str, sock_path: str) -> Optional[pexpect.spawn]:
        binary = AGENT_BINARIES[agent]
        args = list(AGENT_BYPASS_FLAGS.get(agent, []))

        import shutil
        resolved = shutil.which(binary)
        if not resolved:
            self._log(f"  Agent binary '{binary}' not found on PATH, skipping")
            return None

        # Resolve shebang scripts (macOS: /usr/bin/env strips DYLD)
        resolved, args = resolve_script_command(resolved, args)

        # Ensure dyld entitlement on macOS (re-signs if needed)
        if IS_DARWIN:
            if not ensure_dyld_entitlement(resolved):
                self._log(f"  Cannot ensure dyld entitlement for {resolved}, skipping")
                return None

        env = os.environ.copy()

        # Set up interpose
        if IS_DARWIN:
            env["DYLD_INSERT_LIBRARIES"] = self.lib_path
        elif IS_LINUX:
            env["LD_PRELOAD"] = self.lib_path
        env["GREENLIGHT_INTERPOSE_SOCK"] = sock_path
        env["GREENLIGHT_AGENT"] = agent
        env["GREENLIGHT_DEVICE_ID"] = "00000000-0000-0000-0000-000000000000"
        env["GREENLIGHT_SESSION_ID"] = f"test-{random_suffix()}"
        env["GREENLIGHT_PROJECT"] = "gl-test"

        # Enable interpose debug logging
        interpose_log = os.path.join(cwd, "interpose.log")
        env["GREENLIGHT_INTERPOSE_LOG"] = interpose_log

        self._log(f"  Spawning: {resolved} {' '.join(args)}")
        self._log(f"  CWD: {cwd}")
        self._log(f"  Socket: {sock_path}")
        self._log(f"  Interpose log: {interpose_log}")

        try:
            child = pexpect.spawn(
                resolved, args,
                cwd=cwd,
                env=env,
                encoding="utf-8",
                timeout=self.ready_timeout,
                maxread=65536,
                dimensions=(40, 120),
            )
            if self.verbose:
                child.logfile_read = sys.stdout
            return child
        except Exception as e:
            self._log(f"  Failed to spawn {agent}: {e}")
            return None

    def _wait_ready(self, child: pexpect.spawn, agent: str) -> bool:
        self._log(f"  Waiting for {agent} to be ready (timeout: {self.ready_timeout}s)...")

        # First, check for trust/safety prompts that need dismissing
        trust_patterns = TRUST_PROMPT_PATTERNS.get(agent, [])
        if trust_patterns:
            ready_patterns = READY_PATTERNS.get(agent, [r">", r"\$"])
            # Wait for either a trust prompt or the ready prompt
            all_pat = trust_patterns + ready_patterns
            try:
                idx = child.expect(all_pat + [pexpect.TIMEOUT, pexpect.EOF],
                                   timeout=self.ready_timeout)
                if idx < len(trust_patterns):
                    # Trust prompt — dismiss with Enter
                    self._log(f"  Dismissing trust prompt (matched: {trust_patterns[idx]})")
                    child.sendline("")
                    time.sleep(2)
                    # Now wait for the actual ready prompt
                    idx2 = child.expect(ready_patterns + [pexpect.TIMEOUT, pexpect.EOF],
                                        timeout=self.ready_timeout)
                    if idx2 < len(ready_patterns):
                        self._log(f"  Ready (matched: {ready_patterns[idx2]})")
                        time.sleep(1)
                        try:
                            child.read_nonblocking(size=65536, timeout=0.5)
                        except (pexpect.TIMEOUT, pexpect.EOF):
                            pass
                        return True
                    self._log(f"  Timeout after dismissing trust prompt")
                    return False
                elif idx < len(all_pat):
                    # Matched a ready pattern directly (no trust prompt)
                    matched = ready_patterns[idx - len(trust_patterns)]
                    self._log(f"  Ready (matched: {matched})")
                    time.sleep(1)
                    try:
                        child.read_nonblocking(size=65536, timeout=0.5)
                    except (pexpect.TIMEOUT, pexpect.EOF):
                        pass
                    return True
                else:
                    self._log(f"  Timeout/EOF waiting for {agent}")
                    return False
            except Exception as e:
                self._log(f"  Error waiting for {agent}: {e}")
                return False

        # No trust prompt handling — just wait for ready patterns
        patterns = READY_PATTERNS.get(agent, [r">", r"\$"])
        try:
            idx = child.expect(patterns + [pexpect.TIMEOUT, pexpect.EOF],
                               timeout=self.ready_timeout)
            if idx < len(patterns):
                self._log(f"  Ready (matched: {patterns[idx]})")
                time.sleep(1)
                try:
                    child.read_nonblocking(size=65536, timeout=0.5)
                except (pexpect.TIMEOUT, pexpect.EOF):
                    pass
                return True
            elif idx == len(patterns):
                self._log(f"  Timeout waiting for {agent}")
                return False
            else:
                self._log(f"  Agent exited during startup")
                return False
        except Exception as e:
            self._log(f"  Error waiting for {agent}: {e}")
            return False

    def _run_scenario(self, child: pexpect.spawn, agent: str,
                      scenario: TestScenario, handler: InterposeHandler) -> ScenarioResult:
        start = time.time()
        self._log(f"  [{agent}] {scenario.name}: {scenario.prompt[:60]}...")

        # Clear previous requests so we only check new ones
        handler.clear_requests()

        try:
            # Setup if needed (e.g. create test files)
            if scenario.setup:
                scenario.setup(child.cwd if hasattr(child, 'cwd') else '.')

            # Give the agent a moment to be fully ready for input
            time.sleep(2)
            child.sendline(scenario.prompt)

            # Wait for the agent to process. We poll the interpose handler
            # for expected events rather than trying to pattern-match terminal
            # output (which is agent-specific and unreliable).
            deadline = time.time() + SCENARIO_TIMEOUT
            while time.time() < deadline:
                time.sleep(1)
                requests = handler.get_requests()
                passed, detail = scenario.check(requests)
                if passed:
                    duration = time.time() - start
                    self._log(f"    PASS ({duration:.1f}s): {detail}")
                    return ScenarioResult(
                        agent=agent, scenario=scenario.name,
                        tool_type=scenario.tool_type.value,
                        result=TestResult.PASS,
                        detail=detail, duration=duration,
                    )

                if not child.isalive():
                    break

            # Timeout — check one final time
            requests = handler.get_requests()
            passed, detail = scenario.check(requests)
            duration = time.time() - start
            if passed:
                self._log(f"    PASS ({duration:.1f}s): {detail}")
                return ScenarioResult(
                    agent=agent, scenario=scenario.name,
                    tool_type=scenario.tool_type.value,
                    result=TestResult.PASS,
                    detail=detail, duration=duration,
                )

            # Log what we did see for debugging
            types_seen = [r.get("type", "?") for r in requests]
            self._log(f"    FAIL ({duration:.1f}s): {detail}")
            if types_seen:
                self._log(f"    Intercepted types: {types_seen}")
            else:
                self._log(f"    No interpose requests received")

            return ScenarioResult(
                agent=agent, scenario=scenario.name,
                tool_type=scenario.tool_type.value,
                result=TestResult.FAIL,
                detail=detail, duration=duration,
            )

        except Exception as e:
            return ScenarioResult(
                agent=agent, scenario=scenario.name,
                tool_type=scenario.tool_type.value,
                result=TestResult.ERROR,
                detail=str(e), duration=time.time() - start,
            )

    def _cleanup(self, child: pexpect.spawn):
        if not child.isalive():
            return
        self._log("  Shutting down agent...")
        try:
            child.kill(signal.SIGTERM)
            # Wait with a timeout — don't hang forever
            for _ in range(5):
                time.sleep(1)
                if not child.isalive():
                    break
            if child.isalive():
                child.kill(signal.SIGKILL)
                time.sleep(1)
            child.close(force=True)
        except Exception:
            try:
                child.close(force=True)
            except Exception:
                pass

    def _log(self, msg: str):
        print(msg, file=sys.stderr, flush=True)


# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

def print_summary(results: list[ScenarioResult]) -> int:
    print("\n" + "=" * 78)
    print("TEST SUMMARY")
    print("=" * 78)

    agents_seen = []
    by_agent: dict[str, list[ScenarioResult]] = {}
    for r in results:
        if r.agent not in by_agent:
            by_agent[r.agent] = []
            agents_seen.append(r.agent)
        by_agent[r.agent].append(r)

    print(f"\n{'Agent':<10} {'Scenario':<15} {'Tool':<12} {'Result':<8} "
          f"{'Time':>6}  Detail")
    print("-" * 78)

    COLORS = {
        TestResult.PASS: "\033[32m",
        TestResult.FAIL: "\033[31m",
        TestResult.ERROR: "\033[31m",
        TestResult.TIMEOUT: "\033[33m",
        TestResult.SKIP: "\033[36m",
    }
    RESET = "\033[0m"

    counts = {r: 0 for r in TestResult}
    for agent in agents_seen:
        for r in by_agent[agent]:
            color = COLORS.get(r.result, "")
            time_str = f"{r.duration:.1f}s" if r.duration > 0 else "-"
            detail = r.detail[:35] if r.detail else ""
            print(f"{r.agent:<10} {r.scenario:<15} {r.tool_type:<12} "
                  f"{color}{r.result.value:<8}{RESET} {time_str:>6}  {detail}")
            counts[r.result] += 1

    total = len(results)
    print("-" * 78)
    print(f"Total: {total}  |  "
          f"\033[32mPass: {counts[TestResult.PASS]}\033[0m  |  "
          f"\033[31mFail: {counts[TestResult.FAIL]}\033[0m  |  "
          f"\033[31mError: {counts[TestResult.ERROR]}\033[0m  |  "
          f"\033[33mTimeout: {counts[TestResult.TIMEOUT]}\033[0m  |  "
          f"\033[36mSkip: {counts[TestResult.SKIP]}\033[0m")
    print("=" * 78)

    return counts[TestResult.FAIL] + counts[TestResult.ERROR]


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="Test greenlight's interpose interception across agents",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument(
        "--agents", nargs="+", choices=ALL_AGENTS, default=None,
        help=f"Agents to test (default: all). Choices: {', '.join(ALL_AGENTS)}",
    )
    parser.add_argument(
        "--lib", default=None,
        help="Path to interpose library (.dylib or .so)",
    )
    parser.add_argument(
        "--greenlight", default=None,
        help="Path to greenlight binary (for extracting interpose lib)",
    )
    parser.add_argument(
        "--timeout", type=int, default=DEFAULT_READY_TIMEOUT,
        help=f"Agent readiness timeout in seconds (default: {DEFAULT_READY_TIMEOUT})",
    )
    parser.add_argument(
        "--scenarios", nargs="+",
        choices=["file_read", "file_write", "bash", "web_fetch"],
        default=None,
        help="Run only specific scenarios (default: all)",
    )
    parser.add_argument(
        "-v", "--verbose", action="store_true",
        help="Show agent output and interpose requests in real time",
    )
    parser.add_argument(
        "--dry-run", action="store_true",
        help="Show what would be tested without spawning agents",
    )

    args = parser.parse_args()

    # Find interpose library
    lib_path = find_interpose_lib(args.lib, args.greenlight) if not args.dry_run else "(dry run)"

    # Resolve agents
    agents = args.agents or ALL_AGENTS

    # Build scenarios
    all_scenarios = build_scenarios()
    if args.scenarios:
        type_map = {
            "file_read": ToolType.FILE_READ,
            "file_write": ToolType.FILE_WRITE,
            "bash": ToolType.BASH,
            "web_fetch": ToolType.WEB_FETCH,
        }
        selected = {type_map[s] for s in args.scenarios}
        all_scenarios = [s for s in all_scenarios if s.tool_type in selected]

    # Dry run
    if args.dry_run:
        print("DRY RUN — would test the following:\n")
        print(f"  Library:   {lib_path}")
        print(f"  Platform:  {'macOS (DYLD)' if IS_DARWIN else 'Linux (LD_PRELOAD)'}")
        print(f"  Timeout:   {args.timeout}s\n")
        for agent in agents:
            import shutil
            binary = AGENT_BINARIES[agent]
            found = shutil.which(binary)
            status = f"({found})" if found else "(NOT FOUND)"
            print(f"  {agent}: {binary} {status}")
            for s in all_scenarios:
                print(f"    - {s.name}: {s.prompt[:60]}...")
            print()
        print(f"Total: {len(agents)} agents x {len(all_scenarios)} scenarios "
              f"= {len(agents) * len(all_scenarios)} tests")
        return 0

    print("Greenlight Interpose Test Harness")
    print(f"  Library:   {lib_path}")
    print(f"  Platform:  {'macOS (DYLD_INSERT_LIBRARIES)' if IS_DARWIN else 'Linux (LD_PRELOAD)'}")
    print(f"  Agents:    {', '.join(agents)}")
    print(f"  Scenarios: {', '.join(s.name for s in all_scenarios)}")
    print(f"  Timeout:   {args.timeout}s")
    print()

    runner = AgentTestRunner(
        lib_path=lib_path,
        ready_timeout=args.timeout,
        verbose=args.verbose,
    )

    for agent in agents:
        print(f"\n{'=' * 40}")
        print(f"Testing agent: {agent}")
        print(f"{'=' * 40}")
        runner.run_agent(agent, all_scenarios)

    failures = print_summary(runner.results)
    return 1 if failures > 0 else 0


if __name__ == "__main__":
    sys.exit(main())
