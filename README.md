# Greenlight CLI

The command-line companion to [Greenlight](https://aigreenlight.app) — approve AI actions from your phone.

Greenlight CLI connects [Claude Code](https://claude.ai/code) to the Greenlight relay server, letting you review and approve tool calls from the Greenlight iOS app.

## Install

### Prebuilt Binaries

Download a binary from the [releases](https://github.com/dnmfarrell/greenlight-cli/releases) page:

| File | Platform |
|------|----------|
| `greenlight-darwin-amd64` | macOS (Intel) |
| `greenlight-darwin-arm64` | macOS (Apple Silicon) |
| `greenlight-linux-amd64` | Linux (x86_64) |
| `greenlight-linux-arm64` | Linux (ARM64) |

```bash
chmod +x greenlight-*
mv greenlight-darwin-arm64 /usr/local/bin/greenlight  # example for Apple Silicon
```

### Build from Source

```bash
go build -ldflags "-X main.wsURL=wss://permit.dnmfarrell.com/ws" -o greenlight .
```

Requires Go 1.24+. macOS and Linux only.

## Quick Start

```bash
greenlight connect --device-id your-device-id
```

This launches Claude Code and connects to the Greenlight relay server. Approve the session on your phone to begin.

To avoid typing your device ID every time, save it to `~/.greenlight/config`:

```
device_id=your-device-id
```

Your device ID can be found on the "About" tab in the Greenlight app.

## Usage

```
greenlight <command> [flags]
```

### `connect`

Start a Claude Code session with remote relay.

```bash
greenlight connect [flags]
```

| Flag | Description |
|------|-------------|
| `--device-id` | Your device ID (required) |
| `--project` | Project name |
| `--resume` | Resume a previous Claude Code session by ID |

## Configuration

Settings can be provided via flags, environment variables, or a config file. Priority: flags > env vars > config file.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `GREENLIGHT_DEVICE_ID` | Device ID (required) |
| `GREENLIGHT_PROJECT` | Project name |
| `GREENLIGHT_LOG` | Custom log file path |

### Config File

Create `~/.greenlight/config` with key=value pairs:

```
device_id=your-device-id
project=my-project
```

## Learn More

Visit [aigreenlight.app](https://aigreenlight.app) to get started.
