#!/usr/bin/env python3
"""
Harvest realistic agent output into the fuzz seed corpus.

The fuzz targets in fuzz_test.go are far more effective when seeded with real
agent transcripts and real interpose requests than with hand-written examples:
a coverage-guided fuzzer mutates *from* its corpus, so valid structure in the
corpus is what lets it reach deep code paths. This script gathers that corpus.

Two sources:

  1. Existing transcripts (default). Every agent writes transcripts under its
     own state dir (~/.codex, ~/.copilot, ~/.cursor, ~/.gemini, ~/.pi). Your
     day-to-day agent usage is already a richer corpus than any scripted
     prompt matrix. This script scans those dirs, splits each transcript into
     individual entries, and routes them to the matching fuzz target.

  2. Interpose requests (optional, --interpose-corpus DIR). greenlight's
     interpose handler will dump every raw request to GREENLIGHT_CORPUS_DIR
     when that env var is set. To capture fresh wrapper strings from every
     agent, run the scenario harness with it pointed at a scratch dir:

         mkdir /tmp/glcorpus
         GREENLIGHT_CORPUS_DIR=/tmp/glcorpus python3 tests/agent_scenario_test.py
         python3 tests/gather_fuzz_corpus.py --interpose-corpus /tmp/glcorpus

Output goes to testdata/corpus/<target>/, one file per deduplicated seed.
fuzz_test.go's addSeedCorpus() loads these automatically. Commit the result —
it is the regression net and it tracks agent format drift over time.

Every harvested seed is scrubbed of the obvious PII (home path, username,
email addresses, common token shapes) before being written. Scrubbing is
best-effort: review a sample before committing, and never run this against
transcripts containing real secrets.

Not a test. Run on demand, typically as part of release preflight alongside
scripts/update-agents.sh.

Usage:
    python3 tests/gather_fuzz_corpus.py                       # scan ~ state dirs
    python3 tests/gather_fuzz_corpus.py --only codex pi
    python3 tests/gather_fuzz_corpus.py --interpose-corpus /tmp/glcorpus
    python3 tests/gather_fuzz_corpus.py --dry-run -v
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
HOME = Path.home()

# agent name -> (glob under HOME for transcript files, fuzz-corpus target dir)
# The target name matches the addSeedCorpus(f, "<name>", ...) call in fuzz_test.go.
TRANSCRIPT_SOURCES = {
    "codex":   (".codex/sessions",        "codex"),
    "copilot": (".copilot/session-state", "copilot"),
    "cursor":  (".cursor/projects",       "cursor"),
    "gemini":  (".gemini/tmp",            "gemini"),
    "pi":      (".pi/agent/sessions",     "pi"),
}

# Shells whose `-c` argument carries a wrapped command.
SHELLS = {"sh", "bash", "zsh", "dash", "ksh", "fish"}

# Per-target cap so `go test -run Fuzz` (which replays every seed) stays fast.
DEFAULT_CAP = 400


# ---------------------------------------------------------------- scrubbing

def _build_scrubbers() -> list[tuple[re.Pattern, str]]:
    rules: list[tuple[re.Pattern, str]] = []
    home = str(HOME)
    rules.append((re.compile(re.escape(home)), "/home/user"))
    # Generic home-prefix rule: catches other users and the partial/truncated
    # paths agents sometimes write in prose (e.g. "/Users/davidfar").
    rules.append((re.compile(r"/(?:Users|home)/[^/\s\"']*"), "/home/user"))
    user = HOME.name
    if user and len(user) > 2:
        rules.append((re.compile(r"\b" + re.escape(user) + r"\b"), "user"))
    rules.append((re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"),
                  "user@example.com"))
    # Common credential shapes.
    rules.append((re.compile(r"\b(sk|pk)-[A-Za-z0-9_-]{16,}"), r"\1-REDACTED"))
    rules.append((re.compile(r"\bghp_[A-Za-z0-9]{16,}"), "ghp_REDACTED"))
    rules.append((re.compile(r"\bgithub_pat_[A-Za-z0-9_]{20,}"), "github_pat_REDACTED"))
    rules.append((re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}"), "xox-REDACTED"))
    rules.append((re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._-]{12,}"), "Bearer REDACTED"))
    return rules


SCRUBBERS = _build_scrubbers()


def scrub(text: str) -> str:
    for pat, repl in SCRUBBERS:
        text = pat.sub(repl, text)
    return text


# ---------------------------------------------------------------- harvesting

def _newest_files(root: Path, pattern: str, limit: int = 40) -> list[Path]:
    """Return up to `limit` files matching pattern under root, newest first."""
    if not root.is_dir():
        return []
    files = [p for p in root.rglob(pattern) if p.is_file()]
    files.sort(key=lambda p: p.stat().st_mtime, reverse=True)
    return files[:limit]


def harvest_jsonl(root: Path, pattern: str) -> list[str]:
    """Each non-empty line of each matching JSONL file is one seed."""
    seeds: list[str] = []
    for f in _newest_files(root, pattern):
        try:
            for line in f.read_text(errors="replace").splitlines():
                line = line.strip()
                if line:
                    seeds.append(line)
        except OSError:
            continue
    return seeds


def harvest_gemini(root: Path) -> list[str]:
    """Gemini stores one JSON document per session. Pull out the individual
    message objects — transformGeminiMessage is fuzzed one message at a time."""
    seeds: list[str] = []
    for f in _newest_files(root, "session-*.json"):
        try:
            doc = json.loads(f.read_text(errors="replace"))
        except (OSError, json.JSONDecodeError):
            continue
        msgs = None
        if isinstance(doc, list):
            msgs = doc
        elif isinstance(doc, dict):
            for key in ("messages", "history", "entries"):
                if isinstance(doc.get(key), list):
                    msgs = doc[key]
                    break
        if not msgs:
            continue
        for m in msgs:
            try:
                seeds.append(json.dumps(m, separators=(",", ":")))
            except (TypeError, ValueError):
                continue
    return seeds


def extract_wrapper_command(req: dict) -> str | None:
    """Given a decoded interpose request, recover the wrapped shell command
    string (the input the unwrap* functions consume)."""
    if req.get("type") != "spawn":
        return None
    args = req.get("args") or []
    if not isinstance(args, list):
        return None
    base = os.path.basename(req.get("path", ""))
    # Cursor puts the real command after a literal "--".
    for i, a in enumerate(args):
        if a == "--" and i + 1 < len(args):
            return args[i + 1]
    # A shell's -c argument carries the wrapped command.
    if base in SHELLS:
        for i, a in enumerate(args):
            if a == "-c" and i + 1 < len(args):
                return args[i + 1]
    return None


def harvest_interpose(corpus_dir: Path) -> tuple[list[str], list[str]]:
    """Returns (raw request JSON seeds, wrapper command seeds)."""
    requests: list[str] = []
    wrappers: list[str] = []
    for f in sorted(corpus_dir.glob("*.json")):
        try:
            raw = f.read_text(errors="replace").strip()
            req = json.loads(raw)
        except (OSError, json.JSONDecodeError):
            continue
        requests.append(raw)
        if isinstance(req, dict):
            cmd = extract_wrapper_command(req)
            if cmd:
                wrappers.append(cmd)
    return requests, wrappers


# ---------------------------------------------------------------- writing

def write_corpus(target: str, seeds: list[str], out_root: Path,
                 cap: int, dry_run: bool, verbose: bool) -> int:
    """Scrub, dedup, cap, and write seeds for one fuzz target. Returns count."""
    seen: dict[str, str] = {}
    for s in seeds:
        scrubbed = scrub(s)
        h = hashlib.sha256(scrubbed.encode("utf-8", "replace")).hexdigest()
        seen.setdefault(h, scrubbed)
    # Deterministic selection: sort by hash, keep the first `cap`.
    chosen = sorted(seen.items())[:cap]

    if verbose:
        print(f"  {target}: {len(seeds)} raw -> {len(seen)} unique -> "
              f"{len(chosen)} written (cap {cap})")
    if dry_run:
        return len(chosen)

    out_dir = out_root / target
    out_dir.mkdir(parents=True, exist_ok=True)
    # Drop stale seeds so the committed corpus reflects only this run.
    for old in out_dir.iterdir():
        if old.is_file():
            old.unlink()
    for h, content in chosen:
        (out_dir / h[:16]).write_text(content)
    return len(chosen)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", default=str(REPO_ROOT / "testdata" / "corpus"),
                    help="corpus output root (default: testdata/corpus)")
    ap.add_argument("--interpose-corpus", metavar="DIR",
                    help="directory of captured interpose requests "
                         "(from GREENLIGHT_CORPUS_DIR)")
    ap.add_argument("--only", nargs="+", metavar="AGENT",
                    help="restrict to these agents")
    ap.add_argument("--cap", type=int, default=DEFAULT_CAP,
                    help=f"max seeds per target (default: {DEFAULT_CAP})")
    ap.add_argument("--dry-run", action="store_true",
                    help="report what would be harvested, write nothing")
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args()

    out_root = Path(args.out)
    # target name -> accumulated seeds
    buckets: dict[str, list[str]] = {}

    agents = args.only or list(TRANSCRIPT_SOURCES)
    for agent in agents:
        if agent not in TRANSCRIPT_SOURCES:
            print(f"unknown agent {agent!r}", file=sys.stderr)
            continue
        subdir, target = TRANSCRIPT_SOURCES[agent]
        root = HOME / subdir
        if agent == "gemini":
            seeds = harvest_gemini(root)
        elif agent == "copilot":
            seeds = harvest_jsonl(root, "events.jsonl")
        else:
            seeds = harvest_jsonl(root, "*.jsonl")
        if args.verbose:
            print(f"{agent}: scanned {root} -> {len(seeds)} entries")
        buckets.setdefault(target, []).extend(seeds)

    if args.interpose_corpus:
        cdir = Path(args.interpose_corpus)
        reqs, wraps = harvest_interpose(cdir)
        if args.verbose:
            print(f"interpose: {cdir} -> {len(reqs)} requests, "
                  f"{len(wraps)} wrapper commands")
        buckets.setdefault("interpose", []).extend(reqs)
        buckets.setdefault("wrappers", []).extend(wraps)

    if not buckets:
        print("nothing harvested — no transcripts found", file=sys.stderr)
        return 1

    total = 0
    print(f"{'(dry run) ' if args.dry_run else ''}writing corpus to {out_root}")
    for target in sorted(buckets):
        total += write_corpus(target, buckets[target], out_root,
                              args.cap, args.dry_run, args.verbose)
    print(f"done: {total} seeds across {len(buckets)} targets")
    return 0


if __name__ == "__main__":
    sys.exit(main())
