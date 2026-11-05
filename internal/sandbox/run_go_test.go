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

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sb "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
	fakeagents "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	fakeextensions "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func TestPostRunGoRouterHeadersAndCredentials(t *testing.T) {
	expiry := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var gotHdr http.Header
	var gotBody runGoWireRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/run-go" {
			t.Errorf("got %s %s, want POST /run-go", r.Method, r.URL.Path)
		}
		gotHdr = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(runGoWireResponse{
			Phase:       "run",
			ExitCode:    0,
			Stdout:      "hello\n",
			BuildStdout: "::kode-gopher:tidy::\n",
			BuildMS:     120,
			RunMS:       35,
			Result:      `{"kind":"ok","value":7}`,
			ResultSize:  23,
		})
	}))
	defer ts.Close()

	tgt := &routerTarget{baseURL: ts.URL, sandboxID: "sb-123", namespace: "codemode", port: "8888"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := postRunGo(ctx, ts.Client(), ts.URL, tgt, RunGoRequest{
		Reset:          true,
		PreserveDirs:   []string{"persist"},
		Files:          map[string][]byte{"main.go": []byte("package main\n")},
		BuildCommand:   "go build",
		RunCommand:     "./run",
		Env:            map[string]string{"GOOGLE_CLOUD_PROJECT": "proj"},
		Credentials:    &Credentials{AccessToken: "test-secret-token", Expiry: expiry, Email: "u@example.com"},
		ResultPath:     ".kode-gopher/result.json",
		MaxResultBytes: 256 << 10,
	}, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != "run" || res.Result != `{"kind":"ok","value":7}` || res.BuildMS != 120 || res.RunMS != 35 {
		t.Fatalf("unexpected response: %+v", res)
	}
	if gotHdr.Get("X-Sandbox-ID") != "sb-123" || gotHdr.Get("X-Sandbox-Namespace") != "codemode" || gotHdr.Get("X-Sandbox-Port") != "8888" || gotHdr.Get("X-Sandbox-Timeout") == "" {
		t.Fatalf("router headers missing: %v", gotHdr)
	}
	if !gotBody.Reset || gotBody.PhaseTimeoutMS != 90_000 || gotBody.Credentials == nil || gotBody.Credentials.AccessToken != "test-secret-token" {
		t.Fatalf("wire body mismatch: %+v", gotBody)
	}
}

func TestPostRunGo404LatchesUnsupportedAnd503ClassifiesSessionDead(t *testing.T) {
	var calls atomic.Int32
	ts404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer ts404.Close()

	rc := &routerCapture{rt: &http.Transport{}}
	rc.target.Store(&routerTarget{baseURL: ts404.URL, sandboxID: "sb-1", namespace: "default", port: "8888"})
	s := Session{
		box:    &sb.Sandbox{},
		direct: ts404.Client(),
		router: rc,
	}
	defer s.closeHTTP()

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := s.RunGo(context.Background(), RunGoRequest{BuildCommand: "b", RunCommand: "r"}); !errors.Is(err, ErrRunGoUnsupported) {
			t.Fatalf("attempt %d: got %v, want ErrRunGoUnsupported", attempt, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected 404 latch to hit server once across 2 RunGo calls, got %d", n)
	}

	// 503 must never echo the token and must classify as ErrSessionDead.
	var calls503 atomic.Int32
	ts503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls503.Add(1)
		http.Error(w, `{"message":"pod gone"}`, http.StatusServiceUnavailable)
	}))
	defer ts503.Close()
	_, err := postRunGo(context.Background(), ts503.Client(), ts503.URL, nil, RunGoRequest{
		BuildCommand: "b",
		RunCommand:   "r",
		Credentials:  &Credentials{AccessToken: "test-never-log", Expiry: time.Now().Add(time.Hour)},
	}, time.Second)
	if err == nil {
		t.Fatal("want error on 503")
	}
	if calls503.Load() != 1 {
		t.Fatalf("want 1 attempt (no retry), got %d", calls503.Load())
	}
	if strings.Contains(err.Error(), "test-never-log") {
		t.Fatalf("token leaked in error: %v", err)
	}
	if !errors.Is(classifySessionErr(err), ErrSessionDead) {
		t.Fatalf("503 from run-go should classify as ErrSessionDead, got: %v", err)
	}

	// Mid-request socket drop (EOF) when caller ctx is still live must
	// classify as ErrSessionDead; a caller-canceled ctx must NOT.
	tsDrop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer tsDrop.Close()

	rcDrop := &routerCapture{rt: &http.Transport{}}
	rcDrop.target.Store(&routerTarget{baseURL: tsDrop.URL, sandboxID: "sb-drop", namespace: "default", port: "8888"})
	sDrop := Session{
		box:    &sb.Sandbox{},
		direct: tsDrop.Client(),
		router: rcDrop,
	}
	defer sDrop.closeHTTP()

	if _, err := sDrop.RunGo(context.Background(), RunGoRequest{BuildCommand: "b", RunCommand: "r"}); !errors.Is(err, ErrSessionDead) {
		t.Fatalf("mid-request EOF should classify as ErrSessionDead, got: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sDrop.RunGo(canceledCtx, RunGoRequest{BuildCommand: "b", RunCommand: "r"}); errors.Is(err, ErrSessionDead) {
		t.Fatalf("caller-canceled ctx must not classify as ErrSessionDead, got: %v", err)
	}
}

func TestRunGoRejectsCredentialsOutsideClusterAndBadPaths(t *testing.T) {
	s := &Session{}
	if _, err := s.RunGo(context.Background(), RunGoRequest{
		BuildCommand: "b",
		RunCommand:   "r",
		Credentials:  &Credentials{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)},
	}); err == nil || !strings.Contains(err.Error(), "in-cluster") {
		t.Fatalf("got %v, want in-cluster error", err)
	}

	sIn := &Session{inCluster: true}
	if _, err := sIn.RunGo(context.Background(), RunGoRequest{
		Files:        map[string][]byte{"../bad.go": []byte("x")},
		BuildCommand: "b",
		RunCommand:   "r",
	}); err == nil || !strings.Contains(err.Error(), "invalid segment") {
		t.Fatalf("got %v, want path validation error", err)
	}
}

func TestSessionRunGoOverServeMuxRouter(t *testing.T) {
	// Upstream sandbox-router rewrites header-routed requests using
	// decoded r.URL.Path with RawPath="" (proxy.go), turning the SDK's
	// `GET /exists/%2E` (`box.Exists(ctx, ".")`) into `GET /exists/.`
	// at sandbox-server, whose http.ServeMux (`GET /exists/`) answers
	// dot-segment paths with 307 Temporary Redirect -> `/exists/`.
	// Verify Session.RunGo captures router headers in-process without
	// hitting that proxy+ServeMux redirect or sending extra wire probes.
	var backendMux http.ServeMux
	backendMux.HandleFunc("GET /exists/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"exists": true})
	})
	backendMux.HandleFunc("POST /run-go", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(runGoWireResponse{
			Phase:    "run",
			ExitCode: 0,
			Result:   `{"kind":"ok","value":"routed"}`,
		})
	})
	backendSrv := httptest.NewServer(&backendMux)
	defer backendSrv.Close()
	backendURL, err := url.Parse(backendSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var routerHits atomic.Int32
	var gotSandboxID string
	routerProxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			routerHits.Add(1)
			gotSandboxID = pr.In.Header.Get("X-Sandbox-ID")
			// Match upstream sandbox-router's header-routed Rewrite:
			// assign decoded Path with RawPath="" (stripping %2E encoding).
			pr.Out.URL = &url.URL{
				Scheme: backendURL.Scheme,
				Host:   backendURL.Host,
				Path:   pr.In.URL.Path,
			}
			pr.Out.Host = ""
		},
	}
	var routerMux http.ServeMux
	routerMux.Handle("/", routerProxy)
	routerSrv := httptest.NewServer(&routerMux)
	defer routerSrv.Close()

	sess := openFakeRouterSession(t, routerSrv.URL)
	defer sess.closeHTTP()

	res, err := sess.RunGo(context.Background(), RunGoRequest{
		Reset:        true,
		Files:        map[string][]byte{"main.go": []byte("package main\nfunc main(){}\n")},
		BuildCommand: "go build",
		RunCommand:   "./run",
	})
	if err != nil {
		t.Fatalf("RunGo over ServeMux router failed: %v", err)
	}
	if res.Phase != "run" || res.ExitCode != 0 || res.Result != `{"kind":"ok","value":"routed"}` {
		t.Fatalf("unexpected RunGo result: %+v", res)
	}
	if gotSandboxID != "go-runtime-pool-cm6xx" {
		t.Fatalf("X-Sandbox-ID = %q, want go-runtime-pool-cm6xx", gotSandboxID)
	}
	if hits := routerHits.Load(); hits != 1 {
		t.Fatalf("router HTTP hits = %d, want 1 (single POST /run-go with no extra network probe)", hits)
	}
}

func openFakeRouterSession(t *testing.T, routerURL string) *Session {
	t.Helper()
	ctx := context.Background()
	agentsCS := fakeagents.NewSimpleClientset()         //nolint:staticcheck
	extensionsCS := fakeextensions.NewSimpleClientset() //nolint:staticcheck
	if _, err := agentsCS.AgentsV1beta1().Sandboxes("default").Create(ctx, &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "go-runtime-pool-cm6xx", Namespace: "default"},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:   string(sandboxv1beta1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
			}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := extensionsCS.ExtensionsV1beta1().SandboxClaims("default").Create(ctx, &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-claim-zkq2p", Namespace: "default"},
		Status: extv1beta1.SandboxClaimStatus{
			SandboxStatus: extv1beta1.SandboxStatus{Name: "go-runtime-pool-cm6xx"},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	router, direct := newSessionHTTP(5*time.Second, false)
	opts := sb.Options{
		Namespace:           "default",
		APIURL:              routerURL,
		ServerPort:          serverPort,
		HTTPTransport:       router,
		DisablePodIPRouting: true,
		SandboxReadyTimeout: 2 * time.Second,
		Quiet:               true,
		K8sHelper: &sb.K8sHelper{
			AgentsClient:     agentsCS.AgentsV1beta1(),
			ExtensionsClient: extensionsCS.ExtensionsV1beta1(),
		},
	}
	client, err := sb.NewClient(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	box, err := client.GetSandbox(context.Background(), "sandbox-claim-zkq2p", "default")
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		client:     client,
		box:        box,
		truncate:   TruncateConfig{HeadBytes: defaultHeadBytes, TailBytes: defaultTailBytes},
		perAttempt: 5 * time.Second,
		direct:     direct,
		router:     router,
	}
}
