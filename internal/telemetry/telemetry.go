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
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
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
	defaultCloudRegion    = "us-central1"
	defaultExportInterval = 60 * time.Second
	cloudMonitoringHost   = "telemetry.googleapis.com"
	monitoringWriteScope  = "https://www.googleapis.com/auth/monitoring.write"
	k8sSANamespacePath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
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
	// Falls back to GOOGLE_CLOUD_PROJECT (or GCE metadata when exporting
	// to telemetry.googleapis.com) if empty.
	Project string
	// Location is the cloud region or zone attached to the OTel resource
	// (cloud.region / cloud.availability_zone). Falls back to
	// GOOGLE_CLOUD_LOCATION, CLOUDSDK_COMPUTE_REGION, CLOUDSDK_COMPUTE_ZONE
	// (ignoring "global"), or GCE metadata when exporting to
	// telemetry.googleapis.com if empty.
	Location string
	// ClusterName is the GKE cluster name attached as k8s.cluster.name.
	// Auto-detected from GCE metadata when exporting to
	// telemetry.googleapis.com if empty.
	ClusterName string
	// Namespace is the Kubernetes namespace attached as k8s.namespace.name.
	// Falls back to POD_NAMESPACE (or the in-cluster service account
	// namespace file) if empty.
	Namespace string
	// InstanceID overrides the OTel service.instance.id resource attribute.
	// Falls back to HOSTNAME, os.Hostname(), or "pid-<pid>" if empty.
	InstanceID string
	// ServiceName overrides the OTel service.name resource attribute
	// (default "kode-gopher").
	ServiceName string
	// ServiceVersion overrides the OTel service.version resource attribute
	// (default version.Effective()).
	ServiceVersion string
	// ExportInterval is the periodic reader interval (default 60s).
	ExportInterval time.Duration

	findCredentials func(ctx context.Context, scopes ...string) (*google.Credentials, error)
	detectMetadata  func(ctx context.Context, project, location, cluster string) (string, string, string)
	hostname        func() (string, error)
	readSANamespace func() string
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
	res, project, err := resolveResource(ctx, cfg, ep.isGoogleCloud)
	if err != nil {
		return nil, noopShutdown, err
	}
	opts, err := buildExporterOptions(ctx, cfg, ep, project)
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
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))),
	)
	var once sync.Once
	var shutErr error
	shutdown := func(sctx context.Context) error {
		once.Do(func() { shutErr = mp.Shutdown(sctx) })
		return shutErr
	}
	return NewRecorder(mp.Meter(scopeName)), shutdown, nil
}

func resolveResource(ctx context.Context, cfg Config, isGoogleCloud bool) (*resource.Resource, string, error) {
	project := firstNonEmpty(cfg.Project, os.Getenv("GOOGLE_CLOUD_PROJECT"))
	location := resolveLocation(cfg.Location)
	cluster := strings.TrimSpace(cfg.ClusterName)
	if isGoogleCloud {
		detect := cfg.detectMetadata
		if detect == nil {
			detect = detectGCEMetadata
		}
		project, location, cluster = detect(ctx, project, location, cluster)
		location = cleanCloudLocation(location)
		if project == "" {
			return nil, "", fmt.Errorf("telemetry: exporting to %s requires --project, GOOGLE_CLOUD_PROJECT, or GCE metadata", cloudMonitoringHost)
		}
	}
	return buildResource(cfg, project, location, cluster, isGoogleCloud), project, nil
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

func buildExporterOptions(ctx context.Context, cfg Config, ep parsedEndpoint, project string) ([]otlpmetricgrpc.Option, error) {
	scheme := "https"
	if ep.insecure {
		scheme = "http"
	}
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpointURL(scheme + "://" + ep.hostPort)}
	if ep.insecure || !ep.isGoogleCloud {
		return opts, nil
	}
	findCreds := cfg.findCredentials
	if findCreds == nil {
		findCreds = google.FindDefaultCredentials
	}
	creds, err := findCreds(ctx, monitoringWriteScope)
	if err != nil {
		return nil, fmt.Errorf("telemetry: find Google credentials for %s: %w", cloudMonitoringHost, err)
	}
	if creds == nil || creds.TokenSource == nil {
		return nil, fmt.Errorf("telemetry: Google credentials for %s have no token source", cloudMonitoringHost)
	}
	opts = append(opts, otlpmetricgrpc.WithDialOption(grpc.WithPerRPCCredentials(&tokenSourceRPCCreds{ts: creds.TokenSource})))
	if project != "" {
		opts = append(opts, otlpmetricgrpc.WithHeaders(map[string]string{"x-goog-user-project": project}))
	}
	return opts, nil
}

func detectGCEMetadata(ctx context.Context, project, location, cluster string) (string, string, string) {
	if project != "" && location != "" && cluster != "" {
		return project, location, cluster
	}
	if !metadata.OnGCEWithContext(ctx) {
		return project, location, cluster
	}
	return detectMetadataFromClient(ctx, metadata.NewClient(nil), project, location, cluster)
}

func detectMetadataFromClient(ctx context.Context, c *metadata.Client, project, location, cluster string) (string, string, string) {
	if project != "" && location != "" && cluster != "" {
		return project, location, cluster
	}
	mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if project == "" {
		if p, err := c.GetWithContext(mctx, "project/project-id"); err == nil {
			project = strings.TrimSpace(p)
		}
	}
	if location == "" {
		if loc, err := c.InstanceAttributeValueWithContext(mctx, "cluster-location"); err == nil && cleanCloudLocation(loc) != "" {
			location = cleanCloudLocation(loc)
		} else if z, err := c.ZoneWithContext(mctx); err == nil {
			location = cleanCloudLocation(z)
		}
	}
	if cluster == "" {
		if cl, err := c.InstanceAttributeValueWithContext(mctx, "cluster-name"); err == nil {
			cluster = strings.TrimSpace(cl)
		}
	}
	return project, location, cluster
}

func cleanCloudLocation(v string) string {
	s := strings.TrimSpace(v)
	if strings.EqualFold(s, "global") {
		return ""
	}
	return s
}

func resolveLocation(explicit string) string {
	return firstNonEmpty(
		cleanCloudLocation(explicit),
		cleanCloudLocation(os.Getenv("GOOGLE_CLOUD_LOCATION")),
		cleanCloudLocation(os.Getenv("CLOUDSDK_COMPUTE_REGION")),
		cleanCloudLocation(os.Getenv("CLOUDSDK_COMPUTE_ZONE")),
	)
}

func resolveHostname(hostFn func() (string, error)) string {
	if hostFn == nil {
		hostFn = os.Hostname
	}
	if h, err := hostFn(); err == nil {
		return strings.TrimSpace(h)
	}
	return ""
}

func resolveInstanceID(explicit string, hostFn func() (string, error)) string {
	if id := firstNonEmpty(explicit, os.Getenv("HOSTNAME")); id != "" {
		return id
	}
	if h := resolveHostname(hostFn); h != "" {
		return h
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

func resolveInClusterNamespace(readSA func() string) string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	if strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")) == "" {
		return ""
	}
	if readSA != nil {
		return strings.TrimSpace(readSA())
	}
	b, err := os.ReadFile(k8sSANamespacePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func splitZoneRegion(loc string) (region, zone string) {
	idx := strings.LastIndexByte(loc, '-')
	if idx <= 0 || !hasTwoNonEmptyParts(loc[:idx]) || !isZoneSuffix(loc[idx+1:]) {
		return loc, ""
	}
	return loc[:idx], loc
}

func hasTwoNonEmptyParts(s string) bool {
	i := strings.IndexByte(s, '-')
	return i > 0 && i < len(s)-1 && !strings.Contains(s, "--") && s[0] != '-' && s[len(s)-1] != '-'
}

func isZoneSuffix(s string) bool {
	if len(s) == 1 {
		return s[0] >= 'a' && s[0] <= 'z'
	}
	if len(s) < 4 || !strings.HasPrefix(s, "ai") {
		return false
	}
	last := s[len(s)-1]
	if last < 'a' || last > 'z' {
		return false
	}
	for i := 2; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func appendLocationAttrs(attrs []attribute.KeyValue, location string) []attribute.KeyValue {
	region, zone := splitZoneRegion(strings.TrimSpace(location))
	if region == "" {
		return attrs
	}
	attrs = append(attrs, attribute.String("cloud.region", region))
	if zone != "" {
		attrs = append(attrs, attribute.String("cloud.availability_zone", zone))
	}
	return attrs
}

func buildResource(cfg Config, project, location, cluster string, isGoogleCloud bool) *resource.Resource {
	defaults := buildDefaultAttrs(cfg, project, location, cluster)
	res, _ := resource.Merge(resource.NewSchemaless(defaults...), resource.Environment())
	if overrides := buildExplicitAttrs(cfg); len(overrides) > 0 {
		res, _ = resource.Merge(res, resource.NewSchemaless(overrides...))
	}
	if isGoogleCloud {
		return finalizeGoogleCloudResource(res, cfg, project)
	}
	return res
}

func buildDefaultAttrs(cfg Config, project, location, cluster string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", defaultServiceName),
		attribute.String("service.version", version.Effective()),
		attribute.String("service.instance.id", resolveInstanceID("", cfg.hostname)),
	}
	if project != "" {
		attrs = append(attrs,
			attribute.String("cloud.provider", "gcp"),
			attribute.String("cloud.account.id", project),
			attribute.String("gcp.project_id", project),
		)
	}
	attrs = appendLocationAttrs(attrs, location)
	if cluster != "" {
		attrs = append(attrs,
			attribute.String("cloud.platform", "gcp_kubernetes_engine"),
			attribute.String("k8s.cluster.name", cluster),
		)
	}
	envNS := resolveInClusterNamespace(cfg.readSANamespace)
	if envNS != "" {
		attrs = append(attrs, attribute.String("k8s.namespace.name", envNS))
	}
	inK8sPod := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")) != "" || strings.TrimSpace(os.Getenv("POD_NAMESPACE")) != ""
	if inK8sPod {
		if pod := firstNonEmpty(os.Getenv("HOSTNAME"), resolveHostname(cfg.hostname)); pod != "" {
			attrs = append(attrs, attribute.String("k8s.pod.name", pod))
		}
	}
	return attrs
}

func buildExplicitAttrs(cfg Config) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if s := strings.TrimSpace(cfg.ServiceName); s != "" {
		attrs = append(attrs, attribute.String("service.name", s))
	}
	if s := strings.TrimSpace(cfg.ServiceVersion); s != "" {
		attrs = append(attrs, attribute.String("service.version", s))
	}
	if s := strings.TrimSpace(cfg.InstanceID); s != "" {
		attrs = append(attrs, attribute.String("service.instance.id", s))
	}
	if p := strings.TrimSpace(cfg.Project); p != "" {
		attrs = append(attrs,
			attribute.String("cloud.provider", "gcp"),
			attribute.String("cloud.account.id", p),
			attribute.String("gcp.project_id", p),
		)
	}
	if loc := cleanCloudLocation(cfg.Location); loc != "" {
		attrs = appendLocationAttrs(attrs, loc)
	}
	if cl := strings.TrimSpace(cfg.ClusterName); cl != "" {
		attrs = append(attrs,
			attribute.String("cloud.platform", "gcp_kubernetes_engine"),
			attribute.String("k8s.cluster.name", cl),
		)
	}
	if ns := strings.TrimSpace(cfg.Namespace); ns != "" {
		attrs = append(attrs, attribute.String("k8s.namespace.name", ns))
	}
	return attrs
}

func finalizeGoogleCloudResource(res *resource.Resource, cfg Config, project string) *resource.Resource {
	hasLoc := false
	hasInst := false
	filtered := make([]attribute.KeyValue, 0, res.Len()+3)
	for _, kv := range res.Attributes() {
		k := string(kv.Key)
		v := strings.TrimSpace(kv.Value.AsString())
		if isLocationKey(k) && strings.EqualFold(v, "global") {
			continue
		}
		if k == "gcp.project_id" || k == "cloud.account.id" || k == "cloud.provider" {
			continue
		}
		if isLocationKey(k) && v != "" {
			hasLoc = true
		}
		if isInstanceKey(k) && v != "" {
			hasInst = true
		}
		filtered = append(filtered, kv)
	}
	filtered = append(filtered,
		attribute.String("cloud.provider", "gcp"),
		attribute.String("cloud.account.id", project),
		attribute.String("gcp.project_id", project),
	)
	if !hasLoc {
		filtered = append(filtered, attribute.String("cloud.region", defaultCloudRegion))
	}
	if !hasInst {
		filtered = append(filtered, attribute.String("service.instance.id", resolveInstanceID(cfg.InstanceID, cfg.hostname)))
	}
	return resource.NewSchemaless(filtered...)
}

func isLocationKey(k string) bool {
	return k == "location" || k == "cloud.availability_zone" || k == "cloud.region"
}

func isInstanceKey(k string) bool {
	return k == "instance" || k == "service.instance.id" || k == "faas.instance"
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
