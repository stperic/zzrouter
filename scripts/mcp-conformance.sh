#!/bin/sh
# 0.1.16 has no 2026-07-28 server scenarios yet; add --spec-version 2026-07-28 when a release does.
# The suite cannot send an API key. Target a dev coordinator through a local
# key-injecting proxy, or a loopback test setup that supplies the admin key.
set -eu
cd "$(dirname "$0")/.."
exec npx -y @modelcontextprotocol/conformance@0.1.16 server \
  --url "${1:?usage: scripts/mcp-conformance.sh <url>}" \
  --expected-failures scripts/mcp-conformance-baseline.yaml
