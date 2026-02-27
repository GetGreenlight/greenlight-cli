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

echo "Signing $BINARY ..."
codesign --sign "$DEVELOPER_ID_APPLICATION" --options runtime --timestamp "$BINARY"

echo "Creating DMG for $BINARY ..."
dmg_path="${BINARY}.dmg"
staging_dir=$(mktemp -d)
cp "$BINARY" "$staging_dir/greenlight"
hdiutil create -volname "Greenlight" -srcfolder "$staging_dir" \
  -ov -format UDZO "$dmg_path"
rm -rf "$staging_dir"

echo "Notarizing $dmg_path ..."
xcrun notarytool submit "$dmg_path" \
  --apple-id "$APPLE_ID" --team-id "$TEAM_ID" --password "$APP_PASSWORD" --wait
xcrun stapler staple "$dmg_path"

echo "Done: $dmg_path"
