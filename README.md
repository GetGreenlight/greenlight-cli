# remux

**re**mote **mux** — a remote multiplexer for terminal sessions.

Runs a command inside a pseudo-terminal with an optional **bidirectional remote channel** via WebSocket. Local keyboard input and remote injections are multiplexed transparently — the user types normally, and a remote server can inject keystrokes programmatically. When connected, PTY output is also streamed back to the server.

## How it works

```
User keyboard
      │
  Your terminal (outer PTY)
      │
  remux ◄───── WebSocket (bidirectional) ─────► remote server
      │          input ◄── server sends commands
      │          output ──► server sees PTY output
  Inner PTY
      │
  Child process (thinks it has a real terminal)
```

- The child process gets a real PTY — `isatty()` returns true, colors work, readline works
- User sees child output and types normally
- If configured, remux connects outbound to a WebSocket server
  - **Inbound**: server messages are injected into the PTY as keystrokes
  - **Outbound**: PTY output is streamed back to the server
- A mutex ensures keyboard and remote input never interleave
- On disconnect, remux automatically reconnects with exponential backoff

## Build

```bash
go build -o remux .
```

Requires Go 1.22+. Works on macOS and Linux.

## Usage

### With remote WebSocket

```bash
export REMUX_WS_URL=wss://example.com/ws/relay?device=abc123
export REMUX_WS_TOKEN=my-secret-token

./remux -- bash
```
If no command is given after `--`, remux runs `$SHELL` (falling back to `/bin/sh`).

If `REMUX_WS_URL` is not set, remux runs without a remote channel.

## Configuration

| Variable | Required | Description |
|----------|----------|-------------|
| `REMUX_WS_URL` | No | WebSocket server URL (e.g. `wss://host/path?args`). Domain, path, and query parameters are all part of the URL. |
| `REMUX_WS_TOKEN` | No | Auth token sent as `Authorization: Bearer <token>` header on connect. |
| `REMUX_WS_MODE` | No | WebSocket directionality: `rw` (default) — read input from server and write output back; `r` — read input only, no output sent; `w` — write output only, input from server ignored. |
| `REMUX_LOG` | No | Path to log file. Defaults to `/tmp/remux-<pid>.log`. All log output (connection status, errors, reconnections) is written here instead of stderr to avoid polluting the terminal. |
| `REMUX_ID` | Auto | A UUID v4 generated on startup and appended to the WebSocket URL as `relay_id`. Not exported to the child process. |
| `REMUX_EXPORT_*` | No | Variables matching this prefix are re-exported to the child process with the prefix stripped. All other `REMUX_*` vars are stripped from the child environment. Values support `$REMUX_ID` substitution for runtime-generated values. For example, `REMUX_EXPORT_DEVICE_ID=abc` exports `DEVICE_ID=abc`, and `REMUX_EXPORT_SESSION_ID='$REMUX_ID'` exports `SESSION_ID=<generated uuid>`. |

## WebSocket Protocol

### Server → remux (input injection)

Each WebSocket message (text or binary) is injected into the PTY as keystrokes. Newlines are translated to carriage returns (raw mode). A carriage return is appended automatically to submit the input.

Examples of what the server might send:
- `y` — approve a prompt (Enter appended automatically)
- `n` — deny a prompt
- `\x03` — Ctrl-C (interrupt)
- Any arbitrary text

### Remux → server (PTY output)

PTY output is streamed back as binary WebSocket messages. This is the raw byte stream from the child process, including ANSI escape codes, colors, cursor movement, etc. The server can ignore these messages if it only needs to send input, or use `REMUX_WS_MODE=r`.

## Reconnection

If the WebSocket connection drops, remux automatically reconnects:

- Exponential backoff: 1s → 2s → 4s → 8s → 16s → 30s (capped)
- Random jitter (±25%) to avoid thundering herd
- Backoff resets on successful connection
- The child process continues running with keyboard-only input while disconnected
- PTY output sent while disconnected is silently dropped

## Testing locally

A test WebSocket server is included for development:

```bash
# Terminal 1: start the test server
go run ./cmd/testserver

# Terminal 2: start remux connected to it
REMUX_WS_URL=ws://localhost:8080/ws ./remux -- bash
```

In the test server terminal, type text and press Enter to inject it into the PTY. Escape sequences are supported:

| Input | Effect |
|-------|--------|
| `echo hello` | Types and submits `echo hello` |
| `\x03` | Sends Ctrl-C |
| `\x04` | Sends Ctrl-D |
| `\t` | Sends Tab |
| `\\` | Sends literal backslash |

## Notes

- On startup, the outer terminal is put into **raw mode** and restored on exit
- **SIGWINCH** is caught and forwarded so the child always sees the correct window size
- **SIGINT/SIGTERM** are forwarded to the child process group
