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

package telemetry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNilRecorderNoop(t *testing.T) {
	var r *Recorder
	ctx := context.Background()
	r.RecordExec(ctx, 100*time.Millisecond, "run", "ok", "wrapped", false)
	r.RecordBuild(ctx, 50*time.Millisecond, "ok", false)
	r.RecordLookup(ctx, 20*time.Millisecond, true, "ok")
	r.RecordSandboxOpen(ctx, 500*time.Millisecond, true, "ok")
	r.AddActiveSessions(ctx, 1)
	r.AddActiveSessions(ctx, -1)
	r.RecordOAuthEvent(ctx, "signin", "ok")
}

func TestRecorderInstruments(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	r := NewRecorder(mp.Meter(scopeName))
	r.RecordExec(ctx, 1250*time.Millisecond, "run", "ok", "wrapped", true)
	r.RecordBuild(ctx, 800*time.Millisecond, "ok", true)
	r.RecordLookup(ctx, 35*time.Millisecond, true, "ok")
	r.RecordSandboxOpen(ctx, 410*time.Millisecond, true, "ok")
	r.AddActiveSessions(ctx, 2)
	r.AddActiveSessions(ctx, -1)
	r.RecordOAuthEvent(ctx, "signin", "ok")
	r.RecordOAuthEvent(ctx, "refresh", "ok")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
		}
	}
	want := []string{
		"kode_gopher.exec.duration",
		"kode_gopher.exec.build.duration",
		"kode_gopher.lookup.duration",
		"kode_gopher.sandbox.open.duration",
		"kode_gopher.sessions.active",
		"kode_gopher.oauth.events",
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("missing expected metric %q in collected metrics: %v", name, seen)
		}
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		raw      string
		hostPort string
		insecure bool
		isGCP    bool
		wantErr  bool
	}{
		{raw: "https://telemetry.googleapis.com", hostPort: "telemetry.googleapis.com:443", insecure: false, isGCP: true},
		{raw: "telemetry.googleapis.com", hostPort: "telemetry.googleapis.com:443", insecure: false, isGCP: true},
		{raw: "telemetry.googleapis.com:443", hostPort: "telemetry.googleapis.com:443", insecure: false, isGCP: true},
		{raw: "http://localhost:4317", hostPort: "localhost:4317", insecure: true, isGCP: false},
		{raw: "otel-collector:4317", hostPort: "otel-collector:4317", insecure: true, isGCP: false},
		{raw: "https://otel.example.com", hostPort: "otel.example.com:443", insecure: false, isGCP: false},
		{raw: "", wantErr: true},
		{raw: "ftp://otel.example.com", wantErr: true},
		{raw: "https://otel.example.com/v1/metrics", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseEndpoint(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEndpoint(%q) = %+v, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEndpoint(%q) unexpected error: %v", tc.raw, err)
			continue
		}
		if got.hostPort != tc.hostPort || got.insecure != tc.insecure || got.isGoogleCloud != tc.isGCP {
			t.Errorf("parseEndpoint(%q) = %+v, want hostPort=%q insecure=%v isGCP=%v",
				tc.raw, got, tc.hostPort, tc.insecure, tc.isGCP)
		}
	}
}

func TestSetupDisabledAndLocal(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	ctx := context.Background()

	if _, _, err := Setup(ctx, Config{Endpoint: "https://telemetry.googleapis.com"}); err == nil {
		t.Fatal("expected error when exporting to telemetry.googleapis.com without project")
	}

	rec, shutdown, err := Setup(ctx, Config{})
	if err != nil {
		t.Fatalf("Setup(empty): %v", err)
	}
	rec.RecordExec(ctx, 10*time.Millisecond, "run", "ok", "wrapped", false)
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown(empty): %v", err)
	}

	rec2, shutdown2, err := Setup(ctx, Config{
		Endpoint:       "http://127.0.0.1:4317",
		Project:        "test-project",
		ExportInterval: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Setup(local): %v", err)
	}
	rec2.RecordOAuthEvent(ctx, "signin", "ok")
	shutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_ = shutdown2(shutCtx)
}
