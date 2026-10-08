# Configuration and providers

The default configuration directory is `~/.config/zzrouter` on Linux,
`~/Library/Application Support/zzrouter` on macOS, and `%APPDATA%\zzrouter`
on Windows. `ZZROUTER_CONFIG_DIR` overrides it. Service installers use a shared
machine directory: `/etc/zzrouter` on Linux and `%PROGRAMDATA%\zzrouter` on Windows.

Run `zzrouter-node config init` to create configuration. Set the listen address
before starting; the embedded node template binds all interfaces and requires
inference authentication. Installers default to loopback. Keep credentials in
protected environment configuration, rather than copied into examples.

| File | Purpose |
| --- | --- |
| `node.yaml` | Bind, authentication, cluster, model storage and update settings |
| `cli.yaml` | Client address, TLS and credential environment-variable selector |
| `providers/settings.yaml` | Global provider settings |
| `providers/<kind>/<name>/config.yaml` | Provider endpoints, runtime and parameter defaults |
| `providers/<kind>/<name>/schema.yaml` | Extended parameter schema |
| `keys.yaml`, `teams.yaml` | Virtual key hashes and team policy |
| `model_groups.yaml` | Logical model routes |

Provider kinds are `on-demand`, `external`, `cloud` and `registries`.
Shipped provider files and assets are embedded in the node; initialization
materializes this tree. The coordinator owns provider writes. Workers receive
managed copies through peer synchronization.

## Parameter tiers

Effective launch settings resolve from provider defaults, model-family defaults,
model settings, node settings, node/model settings and request overrides, in that
order. Later tiers win. Inspect the result with `zzrouter providers resolved`
or `GET /zzrouter/v1/providers/:name/resolved?model=...&node=...`.

Change settings through `PATCH /zzrouter/v1/providers/:name/parameters` with
`Content-Type: application/merge-patch+json`. A null removes an override;
arrays replace. Workers redirect coordinator-owned writes with HTTP 421 and
`Location`.

A model variant uses `models.<variant>.from: <base>` with optional parameters
and request defaults. Equal launch settings share the base process; distinct
settings get a separate process over the base weights. Variant names cannot
conflict with real weights or catalog aliases. One inheritance level is supported.

See [model features](model_features.md), [managed recipes](typed_provider_recipes.md)
and the [API reference](api.md) for deployment and installation contracts.
