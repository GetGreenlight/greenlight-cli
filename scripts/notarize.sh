#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: notarize.sh <binary-path>" >&2
  exit 1
fi

BINARY="$1"

if [[ ! -f "$BINARY" ]]; then
  echo "Error: $BINARY not found" >&2
  exit 1
fi

for var in DEVELOPER_ID_APPLICATION APPLE_ID TEAM_ID APP_PASSWORD; do
  if [[ -z "${!var:-}" ]]; then
    echo "Error: $var is not set" >&2
    exit 1
  fi
done

if codesign -dvv "$BINARY" 2>&1 | grep -q "Authority=$DEVELOPER_ID_APPLICATION"; then
  echo "Skipping signing (already signed): $BINARY"
else
  echo "Signing $BINARY ..."
  codesign --force --sign "$DEVELOPER_ID_APPLICATION" --options runtime --timestamp \
    -i "com.dnmfarrell.greenlight" "$BINARY"
fi

echo "Verifying signature ..."
codesign --verify --deep --strict "$BINARY"

echo "Creating DMG for $BINARY ..."
dmg_path="${BINARY}.dmg"
staging_dir=$(mktemp -d)
cp "$BINARY" "$staging_dir/greenlight"
hdiutil create -volname "Greenlight" -srcfolder "$staging_dir" \
  -ov -format UDZO "$dmg_path"
rm -rf "$staging_dir"

echo "Submitting $dmg_path for notarization ..."
xcrun notarytool submit "$dmg_path" \
  --apple-id "$APPLE_ID" --team-id "$TEAM_ID" --password "$APP_PASSWORD"

echo ""
echo "Submitted. Check status with:"
echo "  xcrun notarytool info <submission-id> --apple-id \"\$APPLE_ID\" --team-id \"\$TEAM_ID\" --password \"\$APP_PASSWORD\""
echo ""
echo "Once accepted, staple with:"
echo "  xcrun stapler staple $dmg_path"
