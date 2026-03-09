#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
if [[ "$VERSION" == *-dirty ]]; then
  echo "Error: uncommitted changes detected (version: $VERSION). Commit or stash before building." >&2
  exit 1
fi
WS_URL="${WS_URL:-wss://permit.dnmfarrell.com/ws/relay}"
OUTDIR="dist"

mkdir -p "$OUTDIR"

# Build interpose libraries from the greenlight-interpose repo
INTERPOSE_DIR="${INTERPOSE_DIR:-$(cd "$(dirname "$0")/../.." && pwd)/greenlight-interpose}"
if [[ ! -d "$INTERPOSE_DIR" ]]; then
  echo "Error: interpose repo not found at $INTERPOSE_DIR" >&2
  echo "Set INTERPOSE_DIR to the greenlight-interpose checkout." >&2
  exit 1
fi
echo "Building interpose libraries from $INTERPOSE_DIR..."
(cd "$INTERPOSE_DIR" && make clean && ./build-all.sh)
cp "$INTERPOSE_DIR"/libhook-*.gz interpose/

platforms=(
  "darwin amd64"
  "darwin arm64"
  "linux  amd64"
  "linux  arm64"
)

for platform in "${platforms[@]}"; do
  read -r os arch <<< "$platform"
  output="$OUTDIR/greenlight-${os}-${arch}"
  echo "Building $output ..."
  export GOOS="$os" GOARCH="$arch" CGO_ENABLED=0
  if [[ "$os" == "darwin" ]]; then
    export MACOSX_DEPLOYMENT_TARGET=13.0
  else
    unset MACOSX_DEPLOYMENT_TARGET
  fi
  go build -ldflags "-X main.version=$VERSION -X main.wsURL=$WS_URL" -o "$output" .
done

echo "Done. Binaries in $OUTDIR/:"
ls -lh "$OUTDIR"/
