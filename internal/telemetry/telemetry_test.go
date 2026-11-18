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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/metadata"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
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

func clearTelemetryEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_METRICS_INSECURE",
		"OTEL_SERVICE_NAME",
		"OTEL_RESOURCE_ATTRIBUTES",
		"GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_LOCATION",
		"CLOUDSDK_COMPUTE_REGION",
		"CLOUDSDK_COMPUTE_ZONE",
		"POD_NAMESPACE",
		"KUBERNETES_SERVICE_HOST",
		"HOSTNAME",
	} {
		t.Setenv(k, "")
	}
}

func TestSetupDisabledAndLocal(t *testing.T) {
	clearTelemetryEnv(t)
	ctx := context.Background()
	noopDetect := func(_ context.Context, p, l, c string) (string, string, string) { return p, l, c }

	if _, _, err := Setup(ctx, Config{
		Endpoint:       "https://telemetry.googleapis.com",
		detectMetadata: noopDetect,
	}); err == nil {
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

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = shutdown2(shutCtx)
		}(i)
	}
	wg.Wait()
	for i, gotErr := range errs {
		if errors.Is(gotErr, sdkmetric.ErrReaderShutdown) {
			t.Fatalf("shutdown2[%d] returned ErrReaderShutdown: %v", i, gotErr)
		}
		if fmt.Sprint(gotErr) != fmt.Sprint(errs[0]) {
			t.Fatalf("shutdown2[%d] = %v, want consistent %v", i, gotErr, errs[0])
		}
	}
}

func TestSplitZoneRegion(t *testing.T) {
	cases := []struct {
		loc        string
		wantRegion string
		wantZone   string
	}{
		{loc: "", wantRegion: "", wantZone: ""},
		{loc: "a", wantRegion: "a", wantZone: ""},
		{loc: "-a", wantRegion: "-a", wantZone: ""},
		{loc: "--a", wantRegion: "--a", wantZone: ""},
		{loc: "us--a", wantRegion: "us--a", wantZone: ""},
		{loc: "us-central1-", wantRegion: "us-central1-", wantZone: ""},
		{loc: "us-central1", wantRegion: "us-central1", wantZone: ""},
		{loc: "europe-southwest1", wantRegion: "europe-southwest1", wantZone: ""},
		{loc: "us-east-1", wantRegion: "us-east-1", wantZone: ""},
		{loc: "us-central1-a", wantRegion: "us-central1", wantZone: "us-central1-a"},
		{loc: "northamerica-northeast1-c", wantRegion: "northamerica-northeast1", wantZone: "northamerica-northeast1-c"},
		{loc: "u-us-prp1-a", wantRegion: "u-us-prp1", wantZone: "u-us-prp1-a"},
		{loc: "us-central1-ai1a", wantRegion: "us-central1", wantZone: "us-central1-ai1a"},
		{loc: "us-east7-ai12b", wantRegion: "us-east7", wantZone: "us-east7-ai12b"},
		{loc: "us-central1-ai", wantRegion: "us-central1-ai", wantZone: ""},
		{loc: "us-central1-aia", wantRegion: "us-central1-aia", wantZone: ""},
		{loc: "us-central1-ai1", wantRegion: "us-central1-ai1", wantZone: ""},
	}
	for _, tc := range cases {
		gotRegion, gotZone := splitZoneRegion(tc.loc)
		if gotRegion != tc.wantRegion || gotZone != tc.wantZone {
			t.Errorf("splitZoneRegion(%q) = (%q, %q), want (%q, %q)",
				tc.loc, gotRegion, gotZone, tc.wantRegion, tc.wantZone)
		}
	}
}

func TestDetectMetadataFromClient(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	var clusterLoc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Metadata-Flavor", "Google")
		switch r.URL.Path {
		case "/computeMetadata/v1/project/project-id":
			_, _ = w.Write([]byte("  meta-project \n"))
		case "/computeMetadata/v1/instance/attributes/cluster-location":
			if clusterLoc == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(clusterLoc))
		case "/computeMetadata/v1/instance/zone":
			_, _ = w.Write([]byte("projects/123456/zones/us-central1-b\n"))
		case "/computeMetadata/v1/instance/attributes/cluster-name":
			_, _ = w.Write([]byte(" meta-cluster "))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest URL: %v", err)
	}
	t.Setenv("GCE_METADATA_HOST", u.Host)
	client := metadata.NewClient(srv.Client())

	// When all fields are already provided, detectMetadataFromClient (and detectGCEMetadata) makes 0 HTTP requests.
	p, l, c := detectGCEMetadata(ctx, "caller-proj", "europe-west1", "caller-cluster")
	if p != "caller-proj" || l != "europe-west1" || c != "caller-cluster" || hits.Load() != 0 {
		t.Fatalf("early return = (%q, %q, %q), hits=%d", p, l, c, hits.Load())
	}

	// Regional GKE cluster-location + whitespace trimming + preserving caller project.
	clusterLoc = " us-central1 "
	p, l, c = detectMetadataFromClient(ctx, client, "explicit-proj", "", "")
	if p != "explicit-proj" || l != "us-central1" || c != "meta-cluster" {
		t.Fatalf("regional detect = (%q, %q, %q), want (explicit-proj, us-central1, meta-cluster)", p, l, c)
	}

	// Missing cluster-location (404) falls back to instance/zone and detects project-id.
	clusterLoc = ""
	p, l, c = detectMetadataFromClient(ctx, client, "", "", "")
	if p != "meta-project" || l != "us-central1-b" || c != "meta-cluster" {
		t.Fatalf("zone fallback detect = (%q, %q, %q), want (meta-project, us-central1-b, meta-cluster)", p, l, c)
	}

	// cluster-location="global" is ignored and falls back to instance/zone.
	clusterLoc = "global"
	_, l, _ = detectMetadataFromClient(ctx, client, "meta-project", "", "meta-cluster")
	if l != "us-central1-b" {
		t.Fatalf("global cluster-location fallback = %q, want us-central1-b", l)
	}
}

func resourceAttrs(res *resource.Resource) map[string]string {
	out := make(map[string]string)
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

func TestResolveResourcePrometheusTargetLabels(t *testing.T) {
	ctx := context.Background()

	t.Run("gke_pod_regional", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv("HOSTNAME", "kode-gopher-6d4b8c9f7-abcde")
		t.Setenv("POD_NAMESPACE", "codemode")
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

		res, proj, err := resolveResource(ctx, Config{
			Project: "gke-demos-345619",
			detectMetadata: func(_ context.Context, p, l, c string) (string, string, string) {
				if l == "" {
					l = "us-central1"
				}
				if c == "" {
					c = "ap-mcp-server"
				}
				return p, l, c
			},
		}, true)
		if err != nil {
			t.Fatalf("resolveResource: %v", err)
		}
		if proj != "gke-demos-345619" {
			t.Fatalf("project = %q, want gke-demos-345619", proj)
		}
		got := resourceAttrs(res)
		want := map[string]string{
			"service.name":        "kode-gopher",
			"service.instance.id": "kode-gopher-6d4b8c9f7-abcde",
			"gcp.project_id":      "gke-demos-345619",
			"cloud.account.id":    "gke-demos-345619",
			"cloud.provider":      "gcp",
			"cloud.region":        "us-central1",
			"cloud.platform":      "gcp_kubernetes_engine",
			"k8s.cluster.name":    "ap-mcp-server",
			"k8s.namespace.name":  "codemode",
			"k8s.pod.name":        "kode-gopher-6d4b8c9f7-abcde",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("attr[%q] = %q, want %q (all=%v)", k, got[k], v, got)
			}
		}
		if _, hasZone := got["cloud.availability_zone"]; hasZone {
			t.Errorf("unexpected cloud.availability_zone for regional location: %v", got)
		}
	})

	t.Run("gke_pod_zonal_and_sa_namespace", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
		t.Setenv("GOOGLE_CLOUD_LOCATION", "global")

		res, _, err := resolveResource(ctx, Config{
			detectMetadata: func(_ context.Context, p, l, c string) (string, string, string) {
				if p == "" {
					p = "gke-demos-345619"
				}
				if l == "" {
					l = "us-central1-a"
				}
				if c == "" {
					c = "zonal-cluster"
				}
				return p, l, c
			},
			hostname:        func() (string, error) { return "kode-gopher-zonal-1", nil },
			readSANamespace: func() string { return "sa-codemode" },
		}, true)
		if err != nil {
			t.Fatalf("resolveResource: %v", err)
		}
		got := resourceAttrs(res)
		if got["cloud.region"] != "us-central1" || got["cloud.availability_zone"] != "us-central1-a" {
			t.Fatalf("zonal location attrs = (region=%q, zone=%q), want (us-central1, us-central1-a)",
				got["cloud.region"], got["cloud.availability_zone"])
		}
		if got["k8s.namespace.name"] != "sa-codemode" || got["k8s.pod.name"] != "kode-gopher-zonal-1" || got["service.instance.id"] != "kode-gopher-zonal-1" {
			t.Fatalf("in-cluster fallback attrs = %v", got)
		}
	})

	t.Run("workstation_off_gce_and_otel_env_precedence", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv("HOSTNAME", "dev-laptop")
		t.Setenv("GOOGLE_CLOUD_LOCATION", "global")

		res, _, err := resolveResource(ctx, Config{
			Project:        "workstation-proj",
			detectMetadata: func(_ context.Context, p, l, c string) (string, string, string) { return p, l, c },
		}, true)
		if err != nil {
			t.Fatalf("resolveResource: %v", err)
		}
		got := resourceAttrs(res)
		if got["service.instance.id"] != "dev-laptop" {
			t.Errorf("service.instance.id = %q, want dev-laptop", got["service.instance.id"])
		}
		if got["cloud.region"] != defaultCloudRegion {
			t.Errorf("cloud.region = %q, want default %q", got["cloud.region"], defaultCloudRegion)
		}
		if _, ok := got["k8s.pod.name"]; ok {
			t.Errorf("unexpected k8s.pod.name outside Kubernetes: %v", got)
		}

		// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override defaults without adding conflicting cloud.region,
		// while mismatched gcp.project_id, global region, or empty service.instance.id cannot poison telemetry.googleapis.com.
		t.Setenv("OTEL_SERVICE_NAME", "custom-kg-env")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "cloud.availability_zone=europe-west1-b,gcp.project_id=wrong-proj,service.instance.id=")
		resEnv, _, err := resolveResource(ctx, Config{
			Project:        "workstation-proj",
			InstanceID:     "explicit-inst",
			detectMetadata: func(_ context.Context, p, l, c string) (string, string, string) { return p, l, c },
		}, true)
		if err != nil {
			t.Fatalf("resolveResource(otel env): %v", err)
		}
		gotEnv := resourceAttrs(resEnv)
		if gotEnv["service.name"] != "custom-kg-env" {
			t.Errorf("service.name = %q, want custom-kg-env", gotEnv["service.name"])
		}
		if gotEnv["service.instance.id"] != "explicit-inst" {
			t.Errorf("service.instance.id = %q, want explicit-inst", gotEnv["service.instance.id"])
		}
		if gotEnv["gcp.project_id"] != "workstation-proj" {
			t.Errorf("gcp.project_id = %q, want workstation-proj", gotEnv["gcp.project_id"])
		}
		if gotEnv["cloud.availability_zone"] != "europe-west1-b" {
			t.Errorf("cloud.availability_zone = %q, want europe-west1-b", gotEnv["cloud.availability_zone"])
		}
		if _, hasDefaultRegion := gotEnv["cloud.region"]; hasDefaultRegion {
			t.Errorf("unexpected default cloud.region alongside explicit OTEL_RESOURCE_ATTRIBUTES zone: %v", gotEnv)
		}

		clearTelemetryEnv(t)
		resPID, _, err := resolveResource(ctx, Config{
			Project:        "workstation-proj",
			detectMetadata: func(_ context.Context, p, l, c string) (string, string, string) { return p, l, c },
			hostname:       func() (string, error) { return "", errors.New("no hostname") },
		}, true)
		if err != nil {
			t.Fatalf("resolveResource(pid fallback): %v", err)
		}
		gotPID := resourceAttrs(resPID)
		if !strings.HasPrefix(gotPID["service.instance.id"], "pid-") {
			t.Errorf("pid fallback service.instance.id = %q, want pid-*", gotPID["service.instance.id"])
		}
	})
}

func TestSetupGoogleCloud(t *testing.T) {
	clearTelemetryEnv(t)
	ctx := context.Background()
	t.Setenv("HOSTNAME", "kode-gopher-pod-1")
	t.Setenv("POD_NAMESPACE", "codemode")
	// Verify explicit https:// endpoint overrides plaintext OTEL_EXPORTER_OTLP_* env vars without breaking TLS.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	fakeCreds := func(context.Context, ...string) (*google.Credentials, error) {
		return &google.Credentials{
			ProjectID:   "gke-demos-345619",
			TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
		}, nil
	}
	fakeMeta := func(_ context.Context, p, l, c string) (string, string, string) {
		if p == "" {
			p = "gke-demos-345619"
		}
		if l == "" {
			l = "us-central1"
		}
		if c == "" {
			c = "kg-sandbox"
		}
		return p, l, c
	}

	for _, badCreds := range []func(context.Context, ...string) (*google.Credentials, error){
		func(context.Context, ...string) (*google.Credentials, error) { return nil, nil },
		func(context.Context, ...string) (*google.Credentials, error) { return &google.Credentials{}, nil },
	} {
		if _, _, err := Setup(ctx, Config{
			Endpoint:        "https://telemetry.googleapis.com",
			detectMetadata:  fakeMeta,
			findCredentials: badCreds,
		}); err == nil {
			t.Fatal("expected error when credentials lack a token source")
		}
	}

	t.Run("metadata_project_discovery", func(t *testing.T) {
		rec, shutdown, err := Setup(ctx, Config{
			Endpoint:        "https://telemetry.googleapis.com",
			ExportInterval:  10 * time.Minute,
			detectMetadata:  fakeMeta,
			findCredentials: fakeCreds,
		})
		if err != nil {
			t.Fatalf("Setup(google cloud metadata): %v", err)
		}
		rec.RecordOAuthEvent(ctx, "signin", "ok")
		shutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_ = shutdown(shutCtx)
	})

	t.Run("explicit_https_overrides_insecure_env", func(t *testing.T) {
		rec, shutdown, err := Setup(ctx, Config{
			Endpoint:        "https://telemetry.googleapis.com",
			Project:         "gke-demos-345619",
			ExportInterval:  10 * time.Minute,
			detectMetadata:  fakeMeta,
			findCredentials: fakeCreds,
		})
		if err != nil {
			t.Fatalf("Setup(google cloud explicit project): %v", err)
		}
		rec.RecordOAuthEvent(ctx, "signin", "ok")
		shutCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_ = shutdown(shutCtx)
	})

	ts := &tokenSourceRPCCreds{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"})}
	if !ts.RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity() = false, want true")
	}
	md, err := ts.GetRequestMetadata(ctx)
	if err != nil || md["authorization"] != "Bearer tok" {
		t.Fatalf("GetRequestMetadata = (%v, %v)", md, err)
	}
}
