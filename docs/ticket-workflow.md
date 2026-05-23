# Ticket-centric workflow

Long-term direction: shift the mobile app's primary unit from "session" to
"ticket." A ticket is a pointer to an external system (GitHub issue, Linear
ticket, etc.) and aggregates the sessions, transcripts, token usage, PR state,
and approvals that belong to it.

Greenlight does not own ticket metadata. It contributes one piece of unique
signal — *which sessions are working on which ticket* — and overlays that on
top of state read from the external tracker.

Most kanban columns are *observed* from external systems, not commanded:

| Column          | Source of truth                          |
| --------------- | ---------------------------------------- |
| Backlog         | GitHub/Linear                            |
| Ready to work   | GitHub label / Linear status             |
| In progress     | **greenlight** (active session + ticket) |
| In testing      | GitHub PR open                           |
| Merged/Released | GitHub PR merged, git tags               |

## v1 scope

Minimum plumbing to start accumulating ticket-tagged session data and to spawn
new sessions from the phone.

### 1. "new" session control message

Add a `new_session` daemon control frame so the phone can spawn a new session
without an existing session ID. Mirrors the existing `wake` flow but for fresh
sessions.

- Payload: `cwd`, `agent` (optional), `ticket` (optional), `request_id`.
- v1 constraint: phone picks `cwd` + `agent` from an *existing* session in its
  history. No filesystem browser, no default-agent UI. First session in a brand
  new project still has to be started from the CLI.
- Daemon spawns a new terminal window running `greenlight connect` with the
  selected agent/cwd, same mechanism as `wakeSession`.
- Response: `new_session_result` with `request_id`, `success`, `error`.

### 2. `--ticket` flag on `connect`

- `greenlight connect --ticket github:owner/repo#423`
- Exports `GREENLIGHT_TICKET` into the child process env.
- Persisted on the session record (`sessions.go` / `daemon_wake.go`) and
  re-exported on resume.
- Format convention: `<tracker>:<identifier>` (e.g. `github:foo/bar#423`).
  Free-form-but-structured so future trackers don't require schema changes.

### 3. Worktree handling

`greenlight run <ticket>` (or the phone-triggered new-session flow with a
ticket) creates a git worktree before spawning the agent. Deterministic — not
left to the agent.

- Open question: worktree location (`~/greenlight-worktrees/<repo>-<ticket>/`
  vs in-repo `.git/worktrees/`). Decide on first use.

### 4. Minimal GitHub skill

Narrow scope: teach the agent to **fetch a GitHub issue** via the REST API
using the `GITHUB_ACCESS_TOKEN` secret, injected via `greenlight run -e`.
The skill lives in the permit server repo at
`~/permit/skills/greenlight-github-issue/` (not in this repo) and is
delivered to sessions via the `session_started` ack.

Deliberately *not* covered by the skill: opening PRs, commenting on
issues, editing labels. Agents already know how to do generic engineering
work — the only thing that needed teaching is "use the API, not `gh`, and
pull the token from greenlight secrets."

## Out of scope for v1 (revisit after dogfooding)

- Phone-side ticket browser / GitHub overlay UI
- Default-agent config + phone UI to set it
- Filesystem browser for choosing CWD from the phone
- `.greenlight-ticket` file in worktree (auto-load on manual `connect`)
- Multi-agent on one ticket (parallel worktrees, diff comparison)
- Ticket templates / skill chains
- Usage-limit detection + auto-resume scheduling
- Autoresearch + other long-running skills triggered from the phone
- Per-ticket transcript search (server-side index)
- Auto session naming (less valuable once tickets group sessions)

## Implementation order

1. `new_session` control frame end-to-end (daemon → server → iOS).
2. `--ticket` flag, session-store persistence, resume re-export.
3. Worktree creation in the spawn path.
4. GitHub skill (markdown).
5. Dogfood for ~2 weeks before designing the kanban UI.
