#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
if [[ "$VERSION" == *-dirty ]]; then
  echo "Error: uncommitted changes detected (version: $VERSION). Commit or stash before building." >&2
  exit 1
fi
if [[ "$VERSION" != v* ]];then
    VERSION="v$VERSION"
fi
WS_URL="${WS_URL:-wss://api.aigreenlight.app/ws/relay}"
OUTDIR="dist"

mkdir -p "$OUTDIR"

# Verify the prebuilt interpose libraries are present. They ship in this repo
# under interpose/ and are regenerated upstream (permit repo) via
# scripts/cli-release.sh, which is what populated this checkout.
for arch in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do
  if [[ ! -f "interpose/libhook-${arch}.gz" ]]; then
    echo "Error: interpose/libhook-${arch}.gz is missing." >&2
    echo "Regenerate from the permit repo: scripts/cli-release.sh" >&2
    exit 1
  fi
done

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
