#!/usr/bin/env bash
# Writes the license and NOTICE texts of everything compiled into the release
# binaries: the Go standard library and every non-main module, for all
# release platforms. Fails if a module ships no license file.
set -euo pipefail

out=${1:?usage: third-party-notices.sh OUTPUT}
rule=$(printf '=%.0s' {1..72})

# Git Bash on Windows: go reports C:\ paths; find and cat want POSIX ones.
posix() { if command -v cygpath >/dev/null; then cygpath -u "$1"; else printf '%s' "$1"; fi; }

modules=$(
  for goos in linux darwin windows; do
    GOOS=$goos go list -deps -f '{{with .Module}}{{if not .Main}}{{with .Replace}}{{.Path}}@{{.Version}} {{.Dir}}{{else}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}{{end}}{{end}}' ./cmd/...
  done | sort -u
)

{
  echo "Third-party software in zzRouter release binaries"
  echo
  echo "Each component is listed with the license and notice files it publishes."
  printf '\n%s\nGo standard library %s\n\n' "$rule" "$(go env GOVERSION)"
  cat "$(posix "$(go env GOROOT)")/LICENSE"
  while read -r module dir; do
    dir=$(posix "$dir")
    files=$(find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' \) | sort)
    if [ -z "$files" ]; then
      echo "no license file for $module in $dir" >&2
      exit 1
    fi
    printf '\n%s\n%s\n' "$rule" "$module"
    for file in $files; do
      printf '\n'
      cat "$file"
    done
  done <<< "$modules"
} > "$out"
