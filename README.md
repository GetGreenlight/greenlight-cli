# Greenlight CLI

The command-line companion to [Greenlight](https://aigreenlight.app) — approve AI actions from your phone.

Greenlight CLI connects [Claude Code](https://claude.ai/code) to the Greenlight relay server, letting you review and approve tool calls from the Greenlight iOS app.

## Install

### Prebuilt Binaries

Download a binary from the [releases](https://github.com/get-greenlight/greenlight-cli/releases) page:

| File | Platform |
|------|----------|
| `greenlight-darwin-amd64` | macOS (Intel) |
| `greenlight-darwin-arm64` | macOS (Apple Silicon) |
| `greenlight-linux-amd64` | Linux (x86_64) |
| `greenlight-linux-arm64` | Linux (ARM64) |

The Linux binaries also work under WSL2 (Windows Subsystem for Linux).

macOS binaries are codesigned and notarized by Apple, so they work out of the box without Gatekeeper warnings.

```bash
chmod +x greenlight-*
mv greenlight-darwin-arm64 /usr/local/bin/greenlight  # example for Apple Silicon
```

### Install Script

```bash
curl -sSL https://aigreenlight.app/install.sh | bash
```

### Build from Source

```bash
go build -ldflags "-X main.version=VERSION -X main.wsURL=wss://api.aigreenlight.app/ws/relay" -o greenlight .
```

Or use `scripts/build.sh` which auto-detects the version from git tags and builds for all platforms:

```bash
scripts/build.sh
```

Requires Go 1.20+. macOS, Linux, and WSL2.

## Quick Start

Register your device ID (found on the "About" tab in the Greenlight app):

```bash
greenlight register your-device-id
```

Then start a session:

```bash
greenlight connect
```

This launches Claude Code and connects to the Greenlight relay server. Approve the session on your phone to begin.

## Usage

```
greenlight <command> [flags]
```

### `version`

Print the version number and build settings:

```bash
greenlight version
```

### `register`

Register a device ID for the Greenlight app:

```bash
greenlight register <device-id>
```

Writes the device ID to `~/.greenlight/config`.

### `connect`

Start a Claude Code session with remote relay.

```bash
greenlight connect [flags]
```

| Flag | Description |
|------|-------------|
| `--device-id` | Device ID (overrides env and config file) |
| `--project` | Project name (overrides env and config file) |
| `--resume` | Resume a previous Claude Code session by ID |

### Remote Session Kill

The Greenlight app can remotely terminate a running session. When a session is killed, greenlight restores your terminal and prints resume instructions:

```
To resume this conversation use --resume <session-id>
```

## Configuration

Settings can be provided via flags, environment variables, or a config file. Priority: flags > env vars > config file.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `GREENLIGHT_DEVICE_ID` | Device ID (required) |
| `GREENLIGHT_PROJECT` | Project name |
| `GREENLIGHT_LOG` | Custom log file path |

### Config File

The config file at `~/.greenlight/config` is a key=value file. `device_id` is set by `greenlight register`; `project` can be added manually:

```
device_id=your-device-id
project=my-project
```

## Testing

Run the integration tests:

```bash
go test -tags integration -v -timeout 120s
```

The tests compile greenlight with a local test server and exercise CLI basics, hook events, streaming, and the full connect flow.

## Learn More

Visit [aigreenlight.app](https://aigreenlight.app) to get started.

## License

Licensed under the Functional Source License, see LICENSE.txt.
