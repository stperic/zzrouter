# OpenTelemetry tracing

zzRouter has built-in OpenTelemetry instrumentation. Enable tracing and OTLP
export on the coordinator in its protected `node.yaml`, then restart it.
Standalone nodes also serve the coordinator role. Tracing is disabled in installed
defaults. No remote telemetry is sent unless `observability.otlp.endpoint` is set.

Workers reject `observability.tracing.enabled: true` or a nonempty
`observability.otlp.endpoint`, even when the observability master switch is off.
Worker-local Prometheus metrics remain supported with `observability.enabled:
true` and `observability.metrics.enabled: true`; the coordinator can scrape them
over the existing cluster mTLS listener. To switch a tracing/exporting coordinator
to worker mode, stop it, disable tracing, remove the OTLP endpoint and restart in
worker mode. Clearing settings through reload alone does not stop the running
exporter, so a live mode change is rejected until a restart.

For an OTLP HTTP/protobuf collector, including a compatible zzFuse receiver:

```yaml
observability:
  enabled: true
  service_name: zzrouter
  tracing:
    enabled: true
    sample_rate: 0.1
  metrics:
    enabled: true
    otlp_export: false
  otlp:
    protocol: http/protobuf
    endpoint: "https://collector.example/otlp"
    headers:
      Authorization: "Bearer <collector-ingestion-key>"
```

The exporter sends traces to `/otlp/v1/traces`. When remote metrics are enabled,
it sends them to `/otlp/v1/metrics`. Use the receiver's actual base URL and required
header name. A terminal `/v1/traces` or `/v1/metrics` is normalized to the base;
repeated signal suffixes and noncanonical path escapes are rejected. Credentials,
query strings and fragments do not belong in the endpoint URL.

For gRPC, keep the same settings and replace the OTLP block with:

```yaml
  otlp:
    protocol: grpc
    endpoint: "collector.example:4317"
    headers:
      X-API-Key: "<collector-ingestion-key>"
```

`grpc` is the default protocol. gRPC uses TLS unless `insecure: true` is explicitly
set for a plaintext receiver. HTTP uses the URL's scheme; HTTPS together with
`insecure: true` is rejected. SDK certificate settings can configure a private CA.

Header values are collector ingestion credentials, separate from router API keys
and backend query/login credentials. Keep actual values outside Git in the
protected node configuration. Explicit node headers override SDK environment
headers, including an explicitly empty header map. An endpoint supplied only
through SDK environment variables does not enable remote export.

`metrics.otlp_export: false` retains local Prometheus metrics while disabling
remote metric push, which is useful for trace-only receivers. Omitting this setting
preserves remote metrics when metrics and an endpoint are enabled. `/metrics` stays
on the node's existing HTTP surface.

Sampling applies to new roots. `sample_rate: 1` records every new root and `0`
records none; valid sampled or unsampled W3C parents keep their sampling decision.
An instrumented agent should send `traceparent` and, when appropriate, `tracestate`
to connect its trace to router HTTP spans and client dispatch attempts.

Attempts cover direct proxy sends, fallback/retry dispatch, local run relays,
worker relays and realtime connections. Realtime spans cover connection duration
and handshake status/errors; per-frame and terminal WebSocket error reporting is
not included in this delivery. A streaming HTTP attempt stays open through
body consumption. Coordinator attempts include requests to workers over the
existing cluster mTLS listener; workers create no tracing spans and connect to no
collector. MCP forwards only serialized trace context into its value-free REST
request, preserving authentication isolation and cancellation. Caller baggage is
not propagated.

Server spans use route templates, methods and status metadata. The instrumentation
does not export request/response bodies, URL paths or queries, caller headers, or
raw error messages. Existing inference spans retain their current model and usage
metadata. Health and Prometheus scrapes do not create HTTP spans.

Export uses the standard SDK with a nonblocking queue of 1,024 spans, batches of
128, a one-second batch interval and five-second export/retry limits. A full queue
can drop telemetry; collector failure does not wait for recovery in request
handlers. Shutdown attempts to drain within its caller's deadline.

This delivery preserves existing history and accounting behavior. It adds no
content-capture setting, history correlation fields or new cost-accounting
contract. Existing logical inference span coverage varies by API; the common HTTP
and dispatch spans provide the basic request/attempt view. Performance acceptance
measurements are deferred, so these settings carry no measured overhead guarantee.

Configure and activate the coordinator after the collector endpoint and
credential are available. Follow the existing [cluster protocol compatibility
procedure](protocol_versioning.md) before upgrading binaries; activating tracing
on a compatible coordinator is separate from upgrading an incompatible cluster.
