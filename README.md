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
go build -ldflags "-X main.version=1.0.0 -X main.wsURL=wss://permit.dnmfarrell.com/ws/relay" -o greenlight .
```

Or use `scripts/build.sh` which auto-detects the version from git tags and builds for all platforms:

```bash
scripts/build.sh
```

Requires Go 1.19+. macOS and Linux only.

### Install Script

If you have Go 1.19+ installed, you can build from source with a single command:

```bash
curl -sSL https://raw.githubusercontent.com/GetGreenlight/greenlight-cli/main/scripts/install.sh | bash
```

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

The config file at `~/.greenlight/config` is managed by `greenlight register`:

```
device_id=your-device-id
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
