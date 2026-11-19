#!/usr/bin/env bash
# Stage the same installable bundle for local releases and CI archives.
set -euo pipefail

if [ "$#" -ne 4 ]; then
    echo "Usage: stage-release.sh <os-arch> <binary-dir> <output-dir> <third-party-notices>" >&2
    exit 2
fi
platform="$1"
source_dir="$2"
output_dir="$3"
notices="$4"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "$platform" in
    linux-amd64|linux-arm64|darwin-amd64|darwin-arm64) extension=""; installers=(install.sh) ;;
    windows-amd64) extension=".exe"; installers=(install.ps1 clean.ps1) ;;
    *) echo "Unsupported release platform: $platform" >&2; exit 2 ;;
esac

binaries=(zzrouter zzrouter-node zzrouter-launcher)
sources=()
for binary in "${binaries[@]}"; do
    source="$source_dir/$binary$extension"
    if [ ! -f "$source" ]; then source="$source_dir/$binary-$platform$extension"; fi
    if [ ! -f "$source" ]; then echo "Incomplete release: missing $binary for $platform" >&2; exit 1; fi
    sources+=("$source")
done
for license in "$repo_dir/LICENSE" "$repo_dir/NOTICE" "$notices"; do
    if [ ! -f "$license" ]; then echo "Missing release license: $license" >&2; exit 1; fi
done
for installer in "${installers[@]}"; do
    if [ ! -f "$repo_dir/scripts/$installer" ]; then echo "Missing release installer: $installer" >&2; exit 1; fi
done

mkdir -p "$output_dir"
for index in "${!binaries[@]}"; do
    target="$output_dir/${binaries[$index]}$extension"
    if [ ! "${sources[$index]}" -ef "$target" ]; then cp "${sources[$index]}" "$target"; fi
    chmod +x "$target"
done
cp "$repo_dir/LICENSE" "$repo_dir/NOTICE" "$output_dir/"
if [ ! "$notices" -ef "$output_dir/THIRD_PARTY_NOTICES" ]; then cp "$notices" "$output_dir/THIRD_PARTY_NOTICES"; fi
for installer in "${installers[@]}"; do cp "$repo_dir/scripts/$installer" "$output_dir/"; done
if [ "$extension" = "" ]; then chmod +x "$output_dir/install.sh"; fi
