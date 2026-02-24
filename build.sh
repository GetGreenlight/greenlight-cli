#!/usr/bin/env bash
set -euo pipefail

WS_URL="${WS_URL:-wss://permit.dnmfarrell.com/ws/relay}"
OUTDIR="dist"

mkdir -p "$OUTDIR"

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
  GOOS="$os" GOARCH="$arch" go build -ldflags "-X main.wsURL=$WS_URL" -o "$output" .
done

echo "Done. Binaries in $OUTDIR/:"
ls -lh "$OUTDIR"/
