<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="internal/server/web/console/brand/wordmark-dark.svg">
    <img src="internal/server/web/console/brand/wordmark-color.svg" width="320" alt="zzRouter">
  </picture>
</h1>

zzRouter is a least-privilege control plane for AI agents. Its API replaces
SSH for model downloads, provider installation, launches and inference across
Linux, macOS and Windows nodes. Human operators retain authority over drivers,
OS packages and managed-runtime allowlists.

It serves OpenAI, Anthropic Messages and Ollama compatible APIs, with virtual
keys, teams, quotas and model routing. Providers include llama.cpp, vLLM, MLX,
Ollama and cloud APIs.

Use endpoint discovery to check each provider's supported operations. Native
requests retain provider options; translated APIs map supported controls and
refuse behavior they cannot represent. See [API compatibility](docs/api.md#compatibility-and-native-options)
for request formats, worker imports and realtime access.

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
mkdir zzrouter-release
tar -xzf "zzrouter-$PLATFORM.tar.gz" -C zzrouter-release
bash zzrouter-release/install.sh
```

The installer starts a local coordinator and preserves an existing installation's
configuration. macOS starts it at login; Linux uses a systemd service when the
installer runs with `sudo`, or a detached process for a normal user. Put
`~/.local/bin` on your PATH for a user installation.

Run `zzrouter console`, enter the temporary code, and create your owner account.
Use People to invite others and Keys & teams to give applications their own
credentials and shared access rules. Keep all three binaries together: the node
verifies the launcher's build-time SHA256 before using it. For Windows,
headless setup, supported release formats and supervised installations, see
[installation and updates](docs/install.md).

Forgot the owner's password? Run `zzrouter access recover-owner` on the
coordinator computer and follow **Get help signing in** in the console. Recovery
keeps your models, providers, teams and application keys.

## Your first session

1. Install the coordinator on your first computer and run `zzrouter console`.
   Exchange the temporary setup code and create your owner account. People use
   account sign-in; you do not need to copy an administrator API key into a browser.
2. Use the setup checklist to connect a cloud provider or prepare a local provider
   and model. Protected provider credential storage is prepared automatically on
   first node startup. For cloud models, save the key in **Providers → Cloud
   providers**, choose **Test connection**, then use **Models → Discover → Add
   cloud model**. Choose **Try in Chat** and send one message to verify access.
   Cloud models use the coordinator connection and require no local download.
3. Add workers when you need more compute. In **Nodes → Add node**, choose the
   other computer's operating system and follow its installation and pairing
   instructions. A coordinator can also serve as a worker; additional nodes are optional.
4. Create teams with the allowed providers, routes and shared budgets. Invite
   people from **People**, or create a named app key in **Keys & teams**. Team
   details show everyone affected, including pending invitations.
5. Open **Clients → Connect client**. MCP applications can authorize through
   browser sign-in; API applications use a named app key. Before sharing links
   with another device, review **System → General** and configure a reachable
   HTTPS console address. The default localhost address works only on its computer.

Use **System → Access history** to review committed access changes and
**System → Recovery** to inspect backup scope and local commands. Follow the
[recovery guide](docs/recovery.md) for offline backup, checking and staged restore.
Back up every node separately and keep protection-key backups separate from archives.

## Quick start

For an evaluation install, build from source with the Go version in `go.mod` or newer. Source builds require authorized access to the private [common module](docs/shared-installation.md#private-module-access):

```sh
make build
bash scripts/install.sh
zzrouter console
```

The installer defaults to loopback. The [configuration guide](docs/configuration.md)
describes configuration locations. After creating your owner account, use the
console checklist to prepare a provider and model, or use the installed CLI:

```sh
curl -fsS http://127.0.0.1:9090/health
zzrouter providers
zzrouter list
```

Use the CLI or [agent quickstart](docs/agent_quickstart.md) to install an
eligible provider, download weights and launch a model. Use console teams to
define access and budgets, then assign each application its own named key.
Applications supporting MCP browser authorization can use your account instead.

When downloading with `zzrouter deploy`, `--provider` selects the inference
engine, such as `llamacpp`, and `--registry` selects the download source, such as
`huggingface`. Use `--node` to choose the computer and `--watch` to follow progress.

Provider-specific diagnostics use a fixed discovery and execution API, backed by
provider definitions. REST and MCP also expose native connection information for
direct testing or use, with existing visibility rules and without starting a run.
See [provider extensibility](docs/provider-extensibility.md) for the supported
adapters, exact-run requirements and transport limits.

## Reference

- [Configuration and providers](docs/configuration.md)
- [Management and inference APIs](docs/api.md)
- [Provider operations and native connections](docs/provider-extensibility.md)
- [Model features](docs/model_features.md)
- [Managed Python recipes](docs/typed_provider_recipes.md)
- [Security model](docs/security.md) and [vulnerability reporting](SECURITY.md)
- [Backup and recovery](docs/recovery.md)
- [Cluster protocol compatibility](docs/protocol_versioning.md)
- [Contributing](CONTRIBUTING.md) and [changelog](CHANGELOG.md)

Licensed under [Apache 2.0](LICENSE).
