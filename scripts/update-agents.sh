#!/usr/bin/env bash
# Update each supported agent runtime to its latest version. Skips
# agents that aren't installed on this machine. Prints a versions
# summary at the end suitable for pasting into release notes.
#
# Usage:
#   scripts/update-agents.sh              # update everything that's present
#   scripts/update-agents.sh claude pi    # update only the named agents
#   scripts/update-agents.sh --check      # print versions only, no updates
#
# Each agent's installer is well-documented but inconsistent across
# vendors. The functions below capture the canonical install/update
# commands as of 2026; if a vendor changes theirs, edit the relevant
# update_<agent> function.

set -uo pipefail

CHECK_ONLY=0
TARGETS=()

for arg in "$@"; do
    case "$arg" in
        --check) CHECK_ONLY=1 ;;
        --help|-h)
            sed -n '2,/^$/p' "$0" | sed 's/^# \?//'
            exit 0
            ;;
        *) TARGETS+=("$arg") ;;
    esac
done

ALL_AGENTS=(claude codex copilot cursor gemini pi)
if [ ${#TARGETS[@]} -eq 0 ]; then
    TARGETS=("${ALL_AGENTS[@]}")
fi

# ---------- per-agent version + update logic ----------

# Print the version of an installed agent, or "not installed".
version_claude()  { command -v claude  >/dev/null && claude --version 2>/dev/null || echo not-installed; }
version_codex()   { command -v codex   >/dev/null && codex --version 2>/dev/null || echo not-installed; }
version_copilot() { command -v copilot >/dev/null && copilot --version 2>/dev/null || echo not-installed; }
version_cursor()  { command -v cursor-agent >/dev/null && cursor-agent --version 2>/dev/null \
                    || (command -v agent >/dev/null && agent --version 2>/dev/null) \
                    || echo not-installed; }
version_gemini()  { command -v gemini  >/dev/null && gemini --version 2>/dev/null || echo not-installed; }
version_pi()      { command -v pi      >/dev/null && pi --version 2>&1 || echo not-installed; }

# Run the agent's own self-updater if it has one, otherwise re-install
# via the canonical channel.
update_claude() {
    # Claude Code has a built-in installer.
    claude install latest
}
update_codex() {
    # Codex CLI is distributed via npm.
    npm install -g @openai/codex
}
update_copilot() {
    # GitHub Copilot CLI ships as an npm package.
    npm install -g @github/copilot
}
update_cursor() {
    # Cursor Agent uses a curl-piped install script.
    curl -fsSL https://cursor.com/install | bash
}
update_gemini() {
    # Gemini CLI is on npm.
    npm install -g @google/gemini-cli
}
update_pi() {
    # Pi CLI install script.
    curl -fsSL https://pi.dev/install.sh | sh
}

# ---------- driver ----------

contains() {
    local needle="$1"; shift
    for x in "$@"; do [ "$x" = "$needle" ] && return 0; done
    return 1
}

# bash 3.2 (macOS default) lacks associative arrays, so cache versions
# in a temp file: "agent\tversion\n".
VERSIONS_FILE=$(mktemp)
trap 'rm -f "$VERSIONS_FILE"' EXIT

set_version() { printf "%s\t%s\n" "$1" "$2" >> "$VERSIONS_FILE"; }
get_version() { awk -F'\t' -v a="$1" '$1==a {v=$2} END {print v}' "$VERSIONS_FILE"; }

echo "==> capturing initial versions"
for a in "${ALL_AGENTS[@]}"; do
    v=$("version_$a" 2>/dev/null || echo error)
    set_version "$a" "$v"
    printf "  %-8s %s\n" "$a" "$v"
done

if [ "$CHECK_ONLY" -eq 1 ]; then
    exit 0
fi

for a in "${TARGETS[@]}"; do
    if ! contains "$a" "${ALL_AGENTS[@]}"; then
        echo "skip: unknown agent $a" >&2
        continue
    fi
    initial=$(get_version "$a")
    if [ "$initial" = "not-installed" ]; then
        echo "skip: $a not installed"
        continue
    fi
    echo
    echo "==> updating $a"
    if "update_$a"; then
        new=$("version_$a" 2>/dev/null || echo error)
        set_version "$a" "$new"
        echo "  $a -> $new"
    else
        echo "  $a update failed (exit $?)" >&2
        set_version "$a" "$initial (update failed)"
    fi
done

echo
echo "==> final versions (for release notes)"
for a in "${ALL_AGENTS[@]}"; do
    v=$(get_version "$a")
    [ "$v" = "not-installed" ] && continue
    printf "  - %-8s %s\n" "$a" "$v"
done
