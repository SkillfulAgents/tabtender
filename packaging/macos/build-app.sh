#!/bin/bash
# Builds Tabtender.app (SwiftUI menu-bar app + embedded tabtenderd +
# LaunchAgent) and, optionally, a DMG.
#
# Usage: packaging/macos/build-app.sh [--dmg] [--native]
#
#   --dmg     also produce dist/Tabtender-<version>.dmg
#   --native  build only for the host architecture (faster; default is a
#             universal arm64 + x86_64 build)
#
# Environment:
#   VERSION            version string (default: git describe, or "dev")
#   CODESIGN_IDENTITY  signing identity (default "-": ad-hoc). Use a
#                      "Developer ID Application: ..." identity for releases.
#   NOTARY_PROFILE     notarytool keychain profile; when set (with --dmg and a
#                      Developer ID identity) the DMG is notarized and stapled.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DIST="$ROOT/dist"
APP="$DIST/Tabtender.app"
PKG="$ROOT/packaging/macos"

MAKE_DMG=0
NATIVE=0
for arg in "$@"; do
	case "$arg" in
	--dmg) MAKE_DMG=1 ;;
	--native) NATIVE=1 ;;
	*) echo "unknown option: $arg" >&2; exit 2 ;;
	esac
done

VERSION="${VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
BUILD_NUMBER="$(git -C "$ROOT" rev-list --count HEAD 2>/dev/null || echo 1)"
IDENTITY="${CODESIGN_IDENTITY:--}"
LDFLAGS="-s -w -X github.com/SkillfulAgents/tabtender/internal/version.Version=$VERSION"

echo "==> Building Tabtender $VERSION"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Library/LaunchAgents" "$DIST/build"

echo "==> tabtenderd"
build_daemon() { # arch output
	(cd "$ROOT" && CGO_ENABLED=0 GOOS=darwin GOARCH="$1" go build -trimpath -ldflags "$LDFLAGS" -o "$2" ./cmd/tabtenderd)
}
if [ "$NATIVE" = 1 ]; then
	build_daemon "$(go env GOHOSTARCH)" "$APP/Contents/MacOS/tabtenderd"
else
	build_daemon arm64 "$DIST/build/tabtenderd-arm64"
	build_daemon amd64 "$DIST/build/tabtenderd-amd64"
	lipo -create -output "$APP/Contents/MacOS/tabtenderd" "$DIST/build/tabtenderd-arm64" "$DIST/build/tabtenderd-amd64"
fi

echo "==> Tabtender (SwiftUI)"
if [ "$NATIVE" = 1 ]; then
	swift build --package-path "$ROOT/apps/macos" -c release --product Tabtender
	SWIFT_BIN="$(swift build --package-path "$ROOT/apps/macos" -c release --show-bin-path)"
else
	swift build --package-path "$ROOT/apps/macos" -c release --product Tabtender --arch arm64 --arch x86_64
	SWIFT_BIN="$(swift build --package-path "$ROOT/apps/macos" -c release --arch arm64 --arch x86_64 --show-bin-path)"
fi
cp "$SWIFT_BIN/Tabtender" "$APP/Contents/MacOS/Tabtender"

echo "==> Bundle metadata"
sed -e "s/__VERSION__/$VERSION/" -e "s/__BUILD__/$BUILD_NUMBER/" "$PKG/Info.plist" >"$APP/Contents/Info.plist"
cp "$PKG/com.skillfulagents.tabtender.daemon.plist" "$APP/Contents/Library/LaunchAgents/"
plutil -lint "$APP/Contents/Info.plist" "$APP/Contents/Library/LaunchAgents/"*.plist >/dev/null

echo "==> Signing ($([ "$IDENTITY" = "-" ] && echo ad-hoc || echo "$IDENTITY"))"
SIGN_FLAGS=(--force --sign "$IDENTITY")
if [ "$IDENTITY" != "-" ]; then
	SIGN_FLAGS+=(--options runtime --timestamp)
fi
# Inside-out: nested executables first, then the bundle.
codesign "${SIGN_FLAGS[@]}" --identifier com.skillfulagents.tabtender.daemon "$APP/Contents/MacOS/tabtenderd"
codesign "${SIGN_FLAGS[@]}" "$APP"
codesign --verify --strict "$APP"

echo "==> $APP"

if [ "$MAKE_DMG" = 1 ]; then
	DMG="$DIST/Tabtender-$VERSION.dmg"
	STAGE="$DIST/build/dmg"
	rm -rf "$STAGE" "$DMG"
	mkdir -p "$STAGE"
	cp -R "$APP" "$STAGE/"
	ln -s /Applications "$STAGE/Applications"
	hdiutil create -quiet -volname "Tabtender" -srcfolder "$STAGE" -ov -format UDZO "$DMG"
	if [ "$IDENTITY" != "-" ]; then
		codesign --force --sign "$IDENTITY" --timestamp "$DMG"
		if [ -n "${NOTARY_PROFILE:-}" ]; then
			echo "==> Notarizing"
			xcrun notarytool submit "$DMG" --keychain-profile "$NOTARY_PROFILE" --wait
			xcrun stapler staple "$DMG"
		fi
	fi
	echo "==> $DMG"
fi
