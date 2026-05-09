#!/usr/bin/env bash
# Build and run the mock relay server, plus a dev greenlight binary
# wired to it. Two-window workflow:
#
#   window 1: ./scripts/dev-mockserver.sh
#   window 2: GREENLIGHT_DAEMON_SOCK=$TMPDIR/gl-dev.sock \
#               ./greenlight-dev daemon start --foreground
#   window 3: GREENLIGHT_DAEMON_SOCK=$TMPDIR/gl-dev.sock \
#               ./greenlight-dev --agent claude
#
# The mockserver listens on 127.0.0.1:7777 by default and exposes an
# admin API at http://127.0.0.1:7777/_admin (see cmd/mockserver/main.go).
set -euo pipefail

cd "$(dirname "$0")/.."

ADDR="${MOCKSERVER_ADDR:-127.0.0.1:7777}"

echo "building greenlight-dev wired to ws://${ADDR}/ws/relay ..."
go build \
  -ldflags "-X main.version=dev -X main.wsURL=ws://${ADDR}/ws/relay" \
  -o greenlight-dev .

echo "building greenlight-mockserver ..."
go build -o greenlight-mockserver ./cmd/mockserver/

echo
echo "starting mockserver on ${ADDR} (Ctrl-C to stop)"
exec ./greenlight-mockserver --addr "${ADDR}" -v
