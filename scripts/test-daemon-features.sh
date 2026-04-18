#!/bin/bash
# Test daemon features: update_shutdown.
# Requires: a running local server (localhost:8080), a running daemon, and socat.
#
# Usage: ./scripts/test-daemon-features.sh

set -e

echo "=== Test: Update Shutdown (no active instances) ==="
# Test the IPC path directly
RESPONSE=$(echo '{"type":"update_shutdown"}' | socat - UNIX-CONNECT:/tmp/greenlight-daemon.sock 2>/dev/null || echo "CONNECT_FAILED")

if echo "$RESPONSE" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'Type: {d[\"type\"]}')" 2>/dev/null; then
    echo "Response: $RESPONSE"
    if echo "$RESPONSE" | grep -q '"ok"'; then
        echo "Daemon shut down. Restart with: greenlight daemon start"
    fi
    echo "PASS: update_shutdown"
else
    echo "Response: $RESPONSE"
    echo "FAIL: update_shutdown"
fi

echo ""
echo "Done."
