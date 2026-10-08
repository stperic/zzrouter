# zzRouter

zzRouter is a least-privilege control plane for AI agents. Its API replaces
SSH for model downloads, provider installation, launches and inference across
Linux, macOS and Windows nodes. Human operators retain authority over drivers,
OS packages and managed-runtime allowlists.

It serves OpenAI, Anthropic Messages and Ollama compatible APIs, with virtual
keys, teams, quotas and model routing. Providers include llama.cpp, vLLM, MLX,
Ollama and cloud APIs.

## Install

Download a published tag from [Releases](https://github.com/stperic/zzrouter/releases).
Set `VERSION` to that tag and `PLATFORM` to `linux-amd64`, `linux-arm64`,
`darwin-amd64` or `darwin-arm64`. Verify the signed checksum file before installing:

```sh
set -eu
VERSION=vX.Y.Z
PLATFORM=linux-amd64
BASE="https://github.com/stperic/zzrouter/releases/download/$VERSION"
for file in "zzrouter-$PLATFORM.tar.gz" checksums.txt checksums.txt.sigstore.json; do
  curl -fLO "$BASE/$file"
done
cosign verify-blob --new-bundle-format \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/stperic/zzrouter/.github/workflows/release.yaml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
EXPECTED=$(awk -v name="zzrouter-$PLATFORM.tar.gz" '$2 == name {print $1}' checksums.txt)
test -n "$EXPECTED"
printf '%s  %s\n' "$EXPECTED" "zzrouter-$PLATFORM.tar.gz" | shasum -a 256 -c -
tar -xzf "zzrouter-$PLATFORM.tar.gz"
mkdir -p "$HOME/.local/bin"
for bin in zzrouter zzrouter-node zzrouter-launcher; do
  install -m 0755 "$bin-$PLATFORM" "$HOME/.local/bin/$bin"
done
```

Put `~/.local/bin` on your PATH. Keep all three binaries together: the node
verifies the launcher's build-time SHA256 before using it.
For Windows checksum verification and supervised installations, see
[installation and updates](docs/install.md).

## Quick start

For an evaluation install, build from source with the Go version in `go.mod` or newer:

```sh
make build
./zzrouter-node config init
```

Before starting, set `node.bind: 127.0.0.1` in the generated `node.yaml`.
The [configuration guide](docs/configuration.md) describes its location.
Load the generated admin key into your shell through your normal protected
credential mechanism, then:

```sh
./zzrouter-node start
curl -fsS http://127.0.0.1:9090/health
./zzrouter providers
./zzrouter list
```

Use the CLI or [agent quickstart](docs/agent_quickstart.md) to install an
eligible provider, download weights and launch a model. Give each inference
client its own virtual key:

```sh
./zzrouter keys create my-agent --rpm 60
```

## Reference

- [Configuration and providers](docs/configuration.md)
- [Management and inference APIs](docs/api.md)
- [Model features](docs/model_features.md)
- [Managed Python recipes](docs/typed_provider_recipes.md)
- [Security model](docs/security.md) and [vulnerability reporting](SECURITY.md)
- [Cluster protocol compatibility](docs/protocol_versioning.md)
- [Contributing](CONTRIBUTING.md) and [changelog](CHANGELOG.md)

Licensed under [Apache 2.0](LICENSE).
