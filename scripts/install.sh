#!/usr/bin/env bash
set -euo pipefail

REPO="https://github.com/GetGreenlight/greenlight-cli.git"
INSTALL_DIR="/usr/local/bin"
MIN_GO_VERSION="1.19"
WS_URL="wss://permit.dnmfarrell.com/ws/relay"

# Check for git
if ! command -v git &>/dev/null; then
  echo "Error: git is required but not installed." >&2
  exit 1
fi

# Check for go
if ! command -v go &>/dev/null; then
  echo "Error: Go $MIN_GO_VERSION+ is required but not installed." >&2
  echo "Install it from https://go.dev/dl/" >&2
  exit 1
fi

# Check go version
go_version=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | sed 's/go//')
go_major=$(echo "$go_version" | cut -d. -f1)
go_minor=$(echo "$go_version" | cut -d. -f2)
min_major=$(echo "$MIN_GO_VERSION" | cut -d. -f1)
min_minor=$(echo "$MIN_GO_VERSION" | cut -d. -f2)
if [[ "$go_major" -lt "$min_major" ]] || { [[ "$go_major" -eq "$min_major" ]] && [[ "$go_minor" -lt "$min_minor" ]]; }; then
  echo "Error: Go $MIN_GO_VERSION+ is required, but found $go_version." >&2
  echo "Update at https://go.dev/dl/" >&2
  exit 1
fi

# Clone and build
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

echo "Cloning greenlight-cli ..."
git clone --depth 1 "$REPO" "$tmpdir/greenlight-cli"

echo "Building greenlight ..."
cd "$tmpdir/greenlight-cli"
go build -ldflags "-X main.wsURL=$WS_URL" -o greenlight .

# Install
echo "Installing to $INSTALL_DIR (may require sudo) ..."
if [[ -w "$INSTALL_DIR" ]]; then
  mv greenlight "$INSTALL_DIR/greenlight"
else
  sudo mv greenlight "$INSTALL_DIR/greenlight"
fi

echo "Done. Run 'greenlight' to get started."
