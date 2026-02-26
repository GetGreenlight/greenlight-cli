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

# Signing/notarization credentials (required for macOS DMGs)
SIGN_ENABLED=false
if [[ -n "${DEVELOPER_ID_APPLICATION:-}" && -n "${APPLE_ID:-}" && -n "${TEAM_ID:-}" && -n "${APP_PASSWORD:-}" ]]; then
  SIGN_ENABLED=true
else
  echo "Warning: DEVELOPER_ID_APPLICATION, APPLE_ID, TEAM_ID, and/or APP_PASSWORD not set."
  echo "         macOS binaries will not be signed, notarized, or packaged as DMGs."
fi

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
    export MACOSX_DEPLOYMENT_TARGET=12.0
  else
    unset MACOSX_DEPLOYMENT_TARGET
  fi
  go build -ldflags "-X main.version=$VERSION -X main.wsURL=$WS_URL" -o "$output" .

  if [[ "$os" == "darwin" && "$SIGN_ENABLED" == "false" ]]; then
    echo "Ad-hoc signing $output ..."
    codesign -s - -f "$output"
  fi

  if [[ "$os" == "darwin" && "$SIGN_ENABLED" == "true" ]]; then
    echo "Signing $output ..."
    codesign --sign "$DEVELOPER_ID_APPLICATION" --options runtime --timestamp "$output"

    # Notarize the binary (notarytool requires zip/dmg/pkg)
    echo "Notarizing $output ..."
    zip -j "${output}.zip" "$output"
    xcrun notarytool submit "${output}.zip" \
      --apple-id "$APPLE_ID" --team-id "$TEAM_ID" --password "$APP_PASSWORD" --wait
    rm "${output}.zip"

    # Create a simple DMG
    echo "Creating DMG for $output ..."
    dmg_path="${output}.dmg"
    staging_dir=$(mktemp -d)
    cp "$output" "$staging_dir/greenlight"
    hdiutil create -volname "Greenlight" -srcfolder "$staging_dir" \
      -ov -format UDZO "$dmg_path"
    rm -rf "$staging_dir"

    # Sign and notarize the DMG
    codesign --sign "$DEVELOPER_ID_APPLICATION" --timestamp "$dmg_path"
    echo "Notarizing $dmg_path ..."
    xcrun notarytool submit "$dmg_path" \
      --apple-id "$APPLE_ID" --team-id "$TEAM_ID" --password "$APP_PASSWORD" --wait
    xcrun stapler staple "$dmg_path"
  fi
done

echo "Done. Binaries in $OUTDIR/:"
ls -lh "$OUTDIR"/
