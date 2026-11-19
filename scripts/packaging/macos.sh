#!/usr/bin/env bash
# Build a native app/DMG from a complete, matching release bundle.
set -euo pipefail
if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
    echo "Usage: macos.sh <amd64|arm64> <release-bundle> <new-output-dir> <version> [--unsigned]" >&2
    exit 2
fi
[ "$(uname -s)" = Darwin ] || { echo "Run this recipe on macOS with Xcode command-line tools." >&2; exit 1; }
architecture="$1"; bundle="$2"; output="$3"; version="$4"; mode="${5:-signed}"
case "$architecture" in amd64) target=x86_64 ;; arm64) target=arm64 ;; *) echo "Unsupported architecture" >&2; exit 2 ;; esac
case "$version" in *[!0-9A-Za-z.+-]*|'') echo "Invalid version" >&2; exit 2 ;; esac
short_version="$(printf '%s' "$version" | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')"
[ -n "$short_version" ] || { echo "Version must begin with major.minor.patch." >&2; exit 2; }
case "$mode" in signed|--unsigned) ;; *) echo "Unknown packaging mode" >&2; exit 2 ;; esac
if [ "$mode" = signed ]; then
    : "${ZZROUTER_MACOS_SIGN_IDENTITY:?Set the Developer ID Application signing identity}"
    : "${ZZROUTER_MACOS_NOTARY_PROFILE:?Set an existing notarytool keychain profile}"
fi
for file in zzrouter zzrouter-node zzrouter-launcher install.sh LICENSE NOTICE THIRD_PARTY_NOTICES; do
    [ -f "$bundle/$file" ] || { echo "Incomplete release bundle: $file" >&2; exit 1; }
done
if [ "$mode" = signed ]; then
    # Launcher signing must precede the hash embedded in the node build. Never
    # sign it here after that hash has been computed.
    for binary in zzrouter zzrouter-node zzrouter-launcher; do
        codesign --verify --strict "$bundle/$binary"
        codesign -dv "$bundle/$binary" 2>&1 | grep -q '^Authority=Developer ID Application:' || { echo "Release binaries need Developer ID signatures before packaging." >&2; exit 1; }
    done
fi
[ ! -e "$output" ] || { echo "Choose a new output directory." >&2; exit 1; }
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
app="$output/image/Router.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources/release"
cp "$bundle/"* "$app/Contents/Resources/release/"
xcrun swiftc -O -target "$target-apple-macos13.0" -framework AppKit "$repo/scripts/packaging/Router.swift" -o "$app/Contents/MacOS/Router"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.zzrouter.desktop</string>
<key>CFBundleExecutable</key><string>Router</string>
<key>CFBundleName</key><string>Router</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$short_version</string>
<key>CFBundleVersion</key><string>$short_version</string>
<key>ZZRouterReleaseVersion</key><string>$version</string>
<key>LSMinimumSystemVersion</key><string>13.0</string>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
ln -s /Applications "$output/image/Applications"
suffix=""
if [ "$mode" = signed ]; then
    codesign --force --timestamp --options runtime --sign "$ZZROUTER_MACOS_SIGN_IDENTITY" "$app"
else
    suffix="-unsigned"
fi
dmg="$output/zzrouter-macos-$architecture-$version$suffix.dmg"
hdiutil create -quiet -volname "Router" -srcfolder "$output/image" -format UDZO "$dmg"
if [ "$mode" = signed ]; then
    codesign --timestamp --sign "$ZZROUTER_MACOS_SIGN_IDENTITY" "$dmg"
    xcrun notarytool submit "$dmg" --keychain-profile "$ZZROUTER_MACOS_NOTARY_PROFILE" --wait
    xcrun stapler staple "$dmg"
    spctl --assess --type open --context context:primary-signature --verbose "$dmg"
fi
echo "$dmg"
