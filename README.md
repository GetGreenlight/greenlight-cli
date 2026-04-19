# Greenlight CLI

The host-side companion to [Greenlight](https://getgreenlight.github.io/) — approve AI agent actions from your phone.

`greenlight` runs a background daemon on each of your machines. The daemon enrolls the host with the Greenlight relay server, launches agents (Claude Code, Codex, Cursor, etc.) under its supervision, streams their transcripts to your phone, and forwards every permission request to the iOS app for approval.

## Install

### Prebuilt Binaries

Download from the [releases](https://github.com/GetGreenlight/greenlight-cli/releases) page:

| File | Platform |
|------|----------|
| `greenlight-darwin-amd64` | macOS (Intel) |
| `greenlight-darwin-arm64` | macOS (Apple Silicon) |
| `greenlight-linux-amd64` | Linux (x86_64) |
| `greenlight-linux-arm64` | Linux (ARM64) |

The Linux binaries also run under WSL2. macOS binaries are codesigned and notarized.

```bash
chmod +x greenlight-*
mv greenlight-darwin-arm64 /usr/local/bin/greenlight   # example for Apple Silicon
```

### Install Script

```bash
curl -sSL https://getgreenlight.github.io/install.sh | bash
```

### Build from Source

```bash
go build -ldflags "-X main.version=VERSION -X main.wsURL=wss://api.aigreenlight.app/ws/relay" -o greenlight .
```

Or use `scripts/build.sh`, which auto-detects the version from git tags and cross-compiles for every supported platform.

## Quick Start

```bash
# Register this host under your Greenlight account
greenlight register you@example.com

# Launch the background daemon
greenlight daemon start

# Spawn a one-off agent in a fresh scratch directory
greenlight org agent create --adhoc
```

`--adhoc` creates a scratch working directory, a matching organization position, and the agent itself — all in one step — then drops you into `greenlight talk` so you can start chatting. Open the Greenlight iOS app to see the agent, approve its tool calls, and watch its transcript live.

For a perpetual, named agent instead, run `greenlight org agent create` (interactive) or wire the flags directly (see `--help`).

## Commands

```
greenlight <command> [flags]
```

| Command | Purpose |
|---------|---------|
| `register <email>` | Enroll this host under a Greenlight account. Writes `user_id` and `host_id` to `~/.greenlight/config`. |
| `daemon {start,stop,status}` | Manage the per-host background daemon. |
| `org agent create [--adhoc]` | Create an agent. With `--adhoc`, creates a disposable "Scratch" agent in `$HOME/greenlight_agents/scratch/<id>/` and execs into `talk`. |
| `org {agent,position,wd,user,model,org} <list\|get\|create\|update\|delete>` | CRUD on every org entity. |
| `wd create` | Create a bare working directory (without a paired position). |
| `talk [--focus <id>]` | TUI for live transcripts and sending input to active agents. Tab switches focus. |
| `update` | Self-update to the latest release. |
| `version` | Print version and build settings. |

Run any command with `--help` for its flags.

## How It Works

- The daemon holds a single multiplexed WebSocket to the relay server (`/ws/daemon`) and keys every frame by `ai_agent_instance_id`.
- Each agent runs as a child of the daemon inside a PTY, with an interposition library (`DYLD_INSERT_LIBRARIES` / `LD_PRELOAD`) that routes tool-request approvals through the daemon.
- A detached `greenlight stream` subprocess per agent tails the agent's transcript JSONL and forwards it to the daemon via a per-agent bridge file; the daemon forwards each line to the relay.
- The host row's `desired_status` column is flipped to `connected` on graceful daemon start and `disconnected` on graceful stop. Crashes and network drops leave it alone — so the server can tell "user turned it off" from "the host fell over".

## Configuration

Settings can be provided via flags, environment variables, or the config file. Priority: flags > env vars > config.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `GREENLIGHT_DAEMON_HOST_ID` | Override the enrolled host_id on daemon start. |
| `GREENLIGHT_LOG` | Log file path (default `/tmp/greenlight-<pid>.log`; daemon logs to `~/.greenlight/daemon.log`). |

### Config File

`~/.greenlight/config` is a key=value file. `register` writes `user_id` and `host_id` there; other keys can be added manually.

## On-Disk State

- `~/.greenlight/` — daemon config, log, PID file.
- `/tmp/greenlight-daemon.sock` — IPC socket for CLI subcommands.
- `/tmp/greenlight-bridge-<ai_agent_instance_id>` — per-agent transcript bridge file.
- `/tmp/greenlight-stream-<ai_agent_instance_id>.pid` — PID of the detached transcript streamer.
- `/tmp/.gl-<hex>` + `/tmp/gl-<hex>.sock` — extracted interpose library and its IPC socket.
- `$HOME/greenlight_agents/` — default parent for agent working directories; `scratch/<id>/` for adhoc agents.

Greenlight also writes a one-time trust-dialog acceptance into `~/.claude.json` for each new cwd before launching claude, and installs a `GEMINI.md` / `AGENTS.md` / `.cursor/rules/greenlight.mdc` / `.github/copilot-instructions.md` in the agent's cwd depending on the harness.

## Testing

```bash
go test -tags integration -v -timeout 120s ./...
```

The integration tests compile greenlight against a local test server and exercise the daemon IPC, the adhoc-agent flow, transcript streaming, and the graceful-shutdown path.

## Learn More

<https://getgreenlight.github.io/>

## License

Functional Source License — see `LICENSE.txt`.
