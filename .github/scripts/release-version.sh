#!/usr/bin/env bash
set -euo pipefail
version=${GITHUB_REF#refs/tags/v}
pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z.-]+))?(\+([0-9A-Za-z.-]+))?$'
if [[ $GITHUB_REF != refs/tags/v* || ! $version =~ $pattern ]]; then
  echo 'Release requires a v-prefixed semantic version tag' >&2
  exit 1
fi
major=${BASH_REMATCH[1]}
minor=${BASH_REMATCH[2]}
patch=${BASH_REMATCH[3]}
prerelease=${BASH_REMATCH[5]}
identifiers='^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$'
if [[ -n $prerelease ]]; then
  [[ $prerelease =~ $identifiers ]] || exit 1
  IFS=. read -ra parts <<< "$prerelease"
  for part in "${parts[@]}"; do
    if [[ $part =~ ^0[0-9]+$ ]]; then
      echo 'Numeric prerelease identifiers cannot have leading zeros' >&2
      exit 1
    fi
  done
fi
if [[ $version == *+* ]]; then
  [[ ${version#*+} =~ $identifiers ]] || exit 1
fi
printf 'version=%s\nmajor=%s\nminor=%s\npatch=%s\nprerelease=%s\nis_prerelease=%s\n' \
  "$version" "$major" "$minor" "$patch" "$prerelease" "$([[ -n $prerelease ]] && echo true || echo false)" >> "$GITHUB_OUTPUT"
