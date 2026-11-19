#!/usr/bin/env bash
# Optional native signing during the release build, before launcher hashing.
set -euo pipefail
[ "$#" = 2 ] || { echo "Usage: sign-binary.sh <os-arch> <binary>" >&2; exit 2; }
case "$1" in
    darwin-*)
        if [ -n "${ZZROUTER_MACOS_SIGN_IDENTITY:-}" ]; then
            codesign --force --timestamp --options runtime --sign "$ZZROUTER_MACOS_SIGN_IDENTITY" "$2"
            codesign --verify --strict "$2"
        fi
        ;;
    windows-*)
        if [ -n "${ZZROUTER_WINDOWS_SIGN_THUMBPRINT:-}" ]; then
            powershell.exe -NoProfile -File "$(dirname "${BASH_SOURCE[0]}")/sign-windows.ps1" -Binary "$2" -Thumbprint "$ZZROUTER_WINDOWS_SIGN_THUMBPRINT"
        fi
        ;;
    linux-*) ;;
    *) echo "Unsupported signing platform: $1" >&2; exit 2 ;;
esac
