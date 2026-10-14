package observability

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Keep telemetry background work bounded independently of SDK environment defaults.
const (
	spanQueueSize     = 1024
	spanBatchSize     = 128
	spanBatchInterval = time.Second
	exportTimeout     = 5 * time.Second
	metadataLimit     = 512
)

func (c *OTLPConfig) validate() error {
	switch c.Protocol {
	case "":
		c.Protocol = "grpc"
	case "grpc", "http/protobuf":
	default:
		return fmt.Errorf("otlp.protocol must be grpc or http/protobuf")
	}
	if c.Endpoint == "" {
		return nil
	}
	if c.Protocol == "http/protobuf" {
		_, err := c.signalURL("traces")
		return err
	}
	host, port, err := net.SplitHostPort(c.Endpoint)
	n, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || strings.ContainsAny(host, "/@?#") {
		return fmt.Errorf("gRPC otlp.endpoint must be host:port")
	}
	return nil
}

func (c *OTLPConfig) signalURL(signal string) (string, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return "", fmt.Errorf("HTTP otlp.endpoint must be an absolute http(s) base URL without credentials, query, fragment or noncanonical path escapes")
	}
	if c.Insecure && u.Scheme == "https" {
		return "", fmt.Errorf("HTTPS otlp.endpoint conflicts with insecure: true")
	}
	prefix := strings.TrimRight(u.EscapedPath(), "/")
	for _, suffix := range []string{"/v1/traces", "/v1/metrics"} {
		if strings.HasSuffix(prefix, suffix) {
			prefix = strings.TrimSuffix(prefix, suffix)
			break
		}
	}
	if strings.HasSuffix(prefix, "/v1/traces") || strings.HasSuffix(prefix, "/v1/metrics") {
		return "", fmt.Errorf("HTTP otlp.endpoint contains repeated signal paths")
	}
	u.RawPath = prefix + "/v1/" + signal
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return "", fmt.Errorf("HTTP otlp.endpoint contains an invalid path")
	}
	return u.String(), nil
}

func (c *OTLPConfig) grpcURL() string {
	scheme := "https"
	if c.Insecure {
		scheme = "http"
	}
	return scheme + "://" + c.Endpoint
}

func (c *OTLPConfig) traceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	if c.Protocol == "http/protobuf" {
		endpoint, err := c.signalURL("traces")
		if err != nil {
			return nil, err
		}
		return otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(endpoint),
			otlptracehttp.WithHeaders(c.Headers),
			otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf),
			otlptracehttp.WithTimeout(exportTimeout),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
				Enabled: true, InitialInterval: 500 * time.Millisecond,
				MaxInterval: time.Second, MaxElapsedTime: exportTimeout,
			}),
		)
	}
	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpointURL(c.grpcURL()),
		otlptracegrpc.WithHeaders(c.Headers),
		otlptracegrpc.WithTimeout(exportTimeout),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled: true, InitialInterval: 500 * time.Millisecond,
			MaxInterval: time.Second, MaxElapsedTime: exportTimeout,
		}),
	)
}

func (c *OTLPConfig) metricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	if c.Protocol == "http/protobuf" {
		endpoint, err := c.signalURL("metrics")
		if err != nil {
			return nil, err
		}
		return otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(endpoint),
			otlpmetrichttp.WithHeaders(c.Headers),
			otlpmetrichttp.WithTimeout(exportTimeout),
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
				Enabled: true, InitialInterval: 500 * time.Millisecond,
				MaxInterval: time.Second, MaxElapsedTime: exportTimeout,
			}),
		)
	}
	return otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpointURL(c.grpcURL()),
		otlpmetricgrpc.WithHeaders(c.Headers),
		otlpmetricgrpc.WithTimeout(exportTimeout),
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{
			Enabled: true, InitialInterval: 500 * time.Millisecond,
			MaxInterval: time.Second, MaxElapsedTime: exportTimeout,
		}),
	)
}

// BoundMetadata copies a UTF-8-safe prefix so SDK attributes cannot retain oversized strings.
func BoundMetadata(value string) string {
	if len(value) > metadataLimit {
		value = value[:metadataLimit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.Clone(value)
}
