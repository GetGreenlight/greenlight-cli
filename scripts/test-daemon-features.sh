#!/bin/bash
# Test daemon features: session history and wake.
# Requires: a running local server (localhost:8080), a running daemon, and websocat.
#
# Usage: ./scripts/test-daemon-features.sh <device-id> <device-secret>
#
# The device-secret is the secret from the devices table (or empty if none set).
# If websocat is not installed: brew install websocat

set -e

DEVICE_ID="${1:?Usage: $0 <device-id> [device-secret]}"
DEVICE_SECRET="${2:-}"
SERVER="${SERVER:-ws://localhost:8080}"

# Check websocat
if ! command -v websocat &>/dev/null; then
    echo "websocat not found. Install with: brew install websocat"
    exit 1
fi

# Check daemon is running
if ! ../greenlight-cli-daemon/greenlight daemon status &>/dev/null; then
    echo "ERROR: daemon is not running. Start it first with: greenlight connect --device-id $DEVICE_ID"
    exit 1
fi

echo "=== Test 1: Session History ==="
echo "Connecting to device WS as phone simulator..."

# Connect to device WS and send session_history request, read one response
RESPONSE=$(echo '{"type":"session_history"}' | websocat -1 \
    "${SERVER}/ws?device_id=${DEVICE_ID}&secret=${DEVICE_SECRET}" 2>/dev/null)

if echo "$RESPONSE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'Type: {d[\"type\"]}')" 2>/dev/null; then
    echo "Response received:"
    echo "$RESPONSE" | python3 -m json.tool 2>/dev/null || echo "$RESPONSE"
    echo "PASS: session_history"
else
    echo "No valid response received: $RESPONSE"
    echo "FAIL: session_history"
fi

echo ""
echo "=== Test 2: Wake Session (with fake relay ID) ==="
# This should fail gracefully since the relay ID doesn't exist
RESPONSE=$(echo '{"type":"wake_session","relay_id":"nonexistent-relay-id"}' | websocat -1 \
    "${SERVER}/ws?device_id=${DEVICE_ID}&secret=${DEVICE_SECRET}" 2>/dev/null)

if echo "$RESPONSE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'Type: {d[\"type\"]}, Success: {d.get(\"success\",\"?\")}')" 2>/dev/null; then
    echo "Response received:"
    echo "$RESPONSE" | python3 -m json.tool 2>/dev/null || echo "$RESPONSE"
    echo "PASS: wake_session (expected failure)"
else
    echo "No valid response received: $RESPONSE"
    echo "FAIL: wake_session"
fi

echo ""
echo "=== Test 3: Update Shutdown (no active sessions) ==="
# Test the IPC path directly
RESPONSE=$(echo '{"type":"update_shutdown"}' | socat - UNIX-CONNECT:/tmp/greenlight-daemon.sock 2>/dev/null || echo "CONNECT_FAILED")

if echo "$RESPONSE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'Type: {d[\"type\"]}')" 2>/dev/null; then
    echo "Response: $RESPONSE"
    # If the daemon shut down, restart it
    if echo "$RESPONSE" | grep -q '"ok"'; then
        echo "Daemon shut down. Restarting..."
        sleep 1
        ../greenlight-cli-daemon/greenlight daemon start
        sleep 2
    fi
    echo "PASS: update_shutdown"
else
    echo "Response: $RESPONSE"
    echo "FAIL: update_shutdown"
fi

echo ""
echo "Done."
