/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package telemetry exports OpenTelemetry metrics for kode-gopher.
// When no OTLP endpoint is configured (--otel-endpoint or
// OTEL_EXPORTER_OTLP_ENDPOINT), Setup returns a no-op Recorder with no
// background goroutines or network activity.
package telemetry

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/gke-demos/kode-gopher/internal/version"
)

const (
	scopeName             = "github.com/gke-demos/kode-gopher"
	defaultServiceName    = "kode-gopher"
	defaultExportInterval = 60 * time.Second
	cloudMonitoringHost   = "telemetry.googleapis.com"
	monitoringWriteScope  = "https://www.googleapis.com/auth/monitoring.write"
)

// Config configures OpenTelemetry metric export.
type Config struct {
	// Endpoint is the OTLP gRPC collector address (e.g. "localhost:4317",
	// "http://otel-collector:4317", or "https://telemetry.googleapis.com").
	// If empty, Setup checks OTEL_EXPORTER_OTLP_METRICS_ENDPOINT and
	// OTEL_EXPORTER_OTLP_ENDPOINT; if those are also empty, metric export
	// is disabled.
	Endpoint string
	// Project is the Google Cloud project ID attached as gcp.project_id
	// (and x-goog-user-project when exporting to telemetry.googleapis.com).
	// Falls back to GOOGLE_CLOUD_PROJECT if empty.
	Project string
	// ServiceName overrides the OTel service.name resource attribute
	// (default "kode-gopher").
	ServiceName string
	// ServiceVersion overrides the OTel service.version resource attribute
	// (default version.Effective()).
	ServiceVersion string
	// ExportInterval is the periodic reader interval (default 60s).
	ExportInterval time.Duration
}

// Recorder records kode-gopher operational metrics. A nil *Recorder is
// safe to call and acts as a no-op.
type Recorder struct {
	execDuration        metric.Float64Histogram
	buildDuration       metric.Float64Histogram
	lookupDuration      metric.Float64Histogram
	sandboxOpenDuration metric.Float64Histogram
	activeSessions      metric.Int64UpDownCounter
	oauthEvents         metric.Int64Counter
}

// NewRecorder constructs a Recorder bound to m. If m is nil, a no-op
// meter is used.
func NewRecorder(m metric.Meter) *Recorder {
	if m == nil {
		m = noop.NewMeterProvider().Meter(scopeName)
	}
	execDur, _ := m.Float64Histogram(
		"kode_gopher.exec.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Total duration of execute_go_code calls in milliseconds."),
	)
	buildDur, _ := m.Float64Histogram(
		"kode_gopher.exec.build.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Sandbox build duration of execute_go_code calls in milliseconds."),
	)
	lookupDur, _ := m.Float64Histogram(
		"kode_gopher.lookup.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of lookup_package_docs calls in milliseconds."),
	)
	openDur, _ := m.Float64Histogram(
		"kode_gopher.sandbox.open.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Time spent opening a sandbox session in milliseconds."),
	)
	activeSess, _ := m.Int64UpDownCounter(
		"kode_gopher.sessions.active",
		metric.WithUnit("{session}"),
		metric.WithDescription("Number of MCP sessions currently holding an open sandbox."),
	)
	oauthEv, _ := m.Int64Counter(
		"kode_gopher.oauth.events",
		metric.WithUnit("{event}"),
		metric.WithDescription("Count of OAuth 2.1 authorization server events."),
	)
	return &Recorder{
		execDuration:        execDur,
		buildDuration:       buildDur,
		lookupDuration:      lookupDur,
		sandboxOpenDuration: openDur,
		activeSessions:      activeSess,
		oauthEvents:         oauthEv,
	}
}

// Setup initializes an OpenTelemetry MeterProvider and Recorder. When no
// endpoint is set, it returns a no-op Recorder and a no-op shutdown
// function.
func Setup(ctx context.Context, cfg Config) (*Recorder, func(context.Context) error, error) {
	noopShutdown := func(context.Context) error { return nil }
	raw := ResolveEndpoint(cfg.Endpoint)
	if raw == "" {
		return NewRecorder(nil), noopShutdown, nil
	}
	ep, err := parseEndpoint(raw)
	if err != nil {
		return nil, noopShutdown, err
	}
	project := strings.TrimSpace(cfg.Project)
	if project == "" {
		project = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if ep.isGoogleCloud && project == "" {
		return nil, noopShutdown, fmt.Errorf("telemetry: exporting to %s requires --project or GOOGLE_CLOUD_PROJECT", cloudMonitoringHost)
	}
	opts, err := buildExporterOptions(ctx, ep, project)
	if err != nil {
		return nil, noopShutdown, err
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, noopShutdown, fmt.Errorf("telemetry: create OTLP metric exporter: %w", err)
	}
	interval := cfg.ExportInterval
	if interval <= 0 {
		interval = defaultExportInterval
	}
	res := buildResource(cfg, project)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))),
	)
	return NewRecorder(mp.Meter(scopeName)), mp.Shutdown, nil
}

// ResolveEndpoint returns explicit if non-empty, else checks standard
// OTLP environment variables.
func ResolveEndpoint(explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")); s != "" {
		return s
	}
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

type parsedEndpoint struct {
	hostPort      string
	insecure      bool
	isGoogleCloud bool
}

func parseEndpoint(raw string) (parsedEndpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return parsedEndpoint{}, fmt.Errorf("telemetry: empty endpoint")
	}
	var host, port string
	var hasScheme, insecure bool
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return parsedEndpoint{}, fmt.Errorf("telemetry: invalid endpoint %q", raw)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return parsedEndpoint{}, fmt.Errorf("telemetry: endpoint scheme must be http or https, got %q", u.Scheme)
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return parsedEndpoint{}, fmt.Errorf("telemetry: endpoint %q must not contain a path, query, fragment or userinfo", raw)
		}
		hasScheme = true
		insecure = u.Scheme == "http"
		host = u.Hostname()
		port = u.Port()
	} else {
		if strings.ContainsAny(raw, "/?# \t") {
			return parsedEndpoint{}, fmt.Errorf("telemetry: invalid endpoint %q", raw)
		}
		if h, p, err := net.SplitHostPort(raw); err == nil {
			host, port = h, p
		} else {
			host = raw
		}
	}
	if host == "" {
		return parsedEndpoint{}, fmt.Errorf("telemetry: invalid endpoint %q", raw)
	}
	isGCP := strings.EqualFold(host, cloudMonitoringHost)
	if !hasScheme {
		insecure = !isGCP && port != "443"
	}
	if port == "" {
		if insecure {
			port = "4317"
		} else {
			port = "443"
		}
	}
	return parsedEndpoint{
		hostPort:      net.JoinHostPort(host, port),
		insecure:      insecure,
		isGoogleCloud: isGCP,
	}, nil
}

func buildExporterOptions(ctx context.Context, ep parsedEndpoint, project string) ([]otlpmetricgrpc.Option, error) {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(ep.hostPort)}
	if ep.insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
		return opts, nil
	}
	if !ep.isGoogleCloud {
		return opts, nil
	}
	creds, err := google.FindDefaultCredentials(ctx, monitoringWriteScope)
	if err != nil {
		return nil, fmt.Errorf("telemetry: find Google credentials for %s: %w", cloudMonitoringHost, err)
	}
	opts = append(opts, otlpmetricgrpc.WithDialOption(grpc.WithPerRPCCredentials(&tokenSourceRPCCreds{ts: creds.TokenSource})))
	if project != "" {
		opts = append(opts, otlpmetricgrpc.WithHeaders(map[string]string{"x-goog-user-project": project}))
	}
	return opts, nil
}

func buildResource(cfg Config, project string) *resource.Resource {
	svcName := strings.TrimSpace(cfg.ServiceName)
	if svcName == "" {
		svcName = defaultServiceName
	}
	svcVer := strings.TrimSpace(cfg.ServiceVersion)
	if svcVer == "" {
		svcVer = version.Effective()
	}
	attrs := []attribute.KeyValue{
		attribute.String("service.name", svcName),
		attribute.String("service.version", svcVer),
	}
	if project != "" {
		attrs = append(attrs, attribute.String("gcp.project_id", project))
	}
	return resource.NewSchemaless(attrs...)
}

type tokenSourceRPCCreds struct {
	ts oauth2.TokenSource
}

func (c *tokenSourceRPCCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	t, err := c.ts.Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + t.AccessToken}, nil
}

func (c *tokenSourceRPCCreds) RequireTransportSecurity() bool {
	return true
}

var _ credentials.PerRPCCredentials = (*tokenSourceRPCCreds)(nil)

// RecordExec records an execute_go_code invocation duration.
func (r *Recorder) RecordExec(ctx context.Context, d time.Duration, phase, status, mode string, tidied bool) {
	if r == nil {
		return
	}
	r.execDuration.Record(ctx, float64(d.Milliseconds()), metric.WithAttributes(
		attribute.String("phase", phase),
		attribute.String("status", status),
		attribute.String("mode", mode),
		attribute.Bool("tidied", tidied),
	))
}

// RecordBuild records the sandbox build phase duration for an execute_go_code call.
func (r *Recorder) RecordBuild(ctx context.Context, d time.Duration, status string, tidied bool) {
	if r == nil {
		return
	}
	r.buildDuration.Record(ctx, float64(d.Milliseconds()), metric.WithAttributes(
		attribute.String("status", status),
		attribute.Bool("tidied", tidied),
	))
}

// RecordLookup records a lookup_package_docs invocation duration.
func (r *Recorder) RecordLookup(ctx context.Context, d time.Duration, batch bool, status string) {
	if r == nil {
		return
	}
	r.lookupDuration.Record(ctx, float64(d.Milliseconds()), metric.WithAttributes(
		attribute.Bool("batch", batch),
		attribute.String("status", status),
	))
}

// RecordSandboxOpen records the time spent opening a sandbox session.
func (r *Recorder) RecordSandboxOpen(ctx context.Context, d time.Duration, inCluster bool, status string) {
	if r == nil {
		return
	}
	r.sandboxOpenDuration.Record(ctx, float64(d.Milliseconds()), metric.WithAttributes(
		attribute.Bool("in_cluster", inCluster),
		attribute.String("status", status),
	))
}

// AddActiveSessions increments or decrements the active sandbox session count.
func (r *Recorder) AddActiveSessions(ctx context.Context, delta int64) {
	if r == nil {
		return
	}
	r.activeSessions.Add(ctx, delta)
}

// RecordOAuthEvent increments the OAuth event counter.
func (r *Recorder) RecordOAuthEvent(ctx context.Context, event, status string) {
	if r == nil {
		return
	}
	r.oauthEvents.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event", event),
		attribute.String("status", status),
	))
}
