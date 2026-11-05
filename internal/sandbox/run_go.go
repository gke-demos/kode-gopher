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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	sb "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// ErrRunGoUnsupported is returned by Session.RunGo when the connected
// sandbox-server does not implement POST /run-go (for example during a
// rolling upgrade before the pinned sandbox image is refreshed) or when
// no direct/router HTTP endpoint is available on the Session.
var ErrRunGoUnsupported = errors.New("sandbox: server does not support /run-go")

// RunGoRequest collapses reset, multi-file write, build, credentialed
// run, and result-file fetch into a single POST /run-go call.
type RunGoRequest struct {
	Reset          bool
	PreserveDirs   []string
	Files          map[string][]byte
	BuildCommand   string
	RunCommand     string
	Env            map[string]string
	Credentials    *Credentials
	ResultPath     string
	MaxResultBytes int64
	PhaseTimeout   time.Duration
}

// RunGoResult is the decoded, stream-truncated outcome of POST /run-go.
type RunGoResult struct {
	Phase           string
	ExitCode        int
	Stdout          string
	Stderr          string
	BuildStdout     string
	BuildDuration   time.Duration
	RunDuration     time.Duration
	TimedOut        bool
	Result          string
	ResultSize      int64
	ResultReadErr   string
	HasAttachments  bool
	StdoutTruncated bool
	StderrTruncated bool
}

// maxRunGoResponse bounds the decoded JSON response for POST /run-go
// (stdout, stderr, optional build markers, and result.json).
const maxRunGoResponse = 32 << 20

type routerTarget struct {
	baseURL   string
	sandboxID string
	namespace string
	port      string
}

type routerProbeKey struct{}

type routerCapture struct {
	rt     *http.Transport
	target atomic.Pointer[routerTarget]
}

func (rc *routerCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	if id := req.Header.Get("X-Sandbox-ID"); id != "" && req.URL != nil && req.URL.Host != "" {
		rc.target.Store(&routerTarget{
			baseURL:   req.URL.Scheme + "://" + req.URL.Host,
			sandboxID: id,
			namespace: req.Header.Get("X-Sandbox-Namespace"),
			port:      req.Header.Get("X-Sandbox-Port"),
		})
	}
	if req.Context().Value(routerProbeKey{}) != nil {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"exists":true}`)),
			Request:    req,
		}, nil
	}
	return rc.rt.RoundTrip(req)
}

func (rc *routerCapture) CloseIdleConnections() {
	if rc != nil && rc.rt != nil {
		rc.rt.CloseIdleConnections()
	}
}

func newSessionHTTP(perAttempt time.Duration, inCluster bool) (*routerCapture, *http.Client) {
	// Combined build+run calls can span two phase budgets plus overhead.
	headerTimeout := 2*perAttempt + 30*time.Second
	directClient := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: perAttempt}).DialContext,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if inCluster {
		return nil, directClient
	}
	sdkTransport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: perAttempt}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: perAttempt,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &routerCapture{rt: sdkTransport}, directClient
}

// RunGo executes a single-hop build+run+fetch cycle against
// sandbox-server's POST /run-go endpoint.
func (s *Session) RunGo(ctx context.Context, req RunGoRequest) (*RunGoResult, error) {
	if req.Credentials != nil && !s.inCluster {
		return nil, errors.New("credentials need in-cluster connectivity (Options.InCluster)")
	}
	for name := range req.Files {
		if err := validatePath(name); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
	}
	if s.noRunGo.Load() {
		return nil, ErrRunGoUnsupported
	}

	baseURL, routerHdr, err := s.resolveRunGoEndpoint(ctx, req.Credentials != nil)
	if err != nil {
		return nil, err
	}

	phaseTimeout := req.PhaseTimeout
	if phaseTimeout <= 0 {
		phaseTimeout = defaultTimeout
	}
	if s.perAttempt > 0 && phaseTimeout > s.perAttempt {
		phaseTimeout = s.perAttempt
	}
	callTimeout := 2*phaseTimeout + 30*time.Second
	if !s.inCluster && callTimeout > defaultTimeout {
		// sandbox-router caps proxy calls at 5m (--proxy-timeout=300s).
		callTimeout = defaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	wire, err := postRunGo(callCtx, s.direct, baseURL, routerHdr, req, phaseTimeout)
	if errors.Is(err, ErrRunGoUnsupported) {
		s.noRunGo.Store(true)
		return nil, ErrRunGoUnsupported
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: run-go: %w", classifySessionErr(err))
	}

	buildStdout := wire.BuildStdout
	if wire.Phase == "build" && buildStdout == "" {
		buildStdout = wire.Stdout
	}
	stdout, outTrunc := truncate(wire.Stdout, s.truncate)
	stderr, errTrunc := truncate(wire.Stderr, s.truncate)
	return &RunGoResult{
		Phase:           wire.Phase,
		ExitCode:        wire.ExitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		BuildStdout:     buildStdout,
		BuildDuration:   time.Duration(wire.BuildMS) * time.Millisecond,
		RunDuration:     time.Duration(wire.RunMS) * time.Millisecond,
		TimedOut:        wire.TimedOut,
		Result:          wire.Result,
		ResultSize:      wire.ResultSize,
		ResultReadErr:   wire.ResultReadErr,
		HasAttachments:  wire.HasAttachments,
		StdoutTruncated: outTrunc,
		StderrTruncated: errTrunc,
	}, nil
}

func (s *Session) resolveRunGoEndpoint(ctx context.Context, hasCreds bool) (string, *routerTarget, error) {
	if s.inCluster {
		if s.box == nil {
			return "", nil, ErrRunGoUnsupported
		}
		fqdn := s.box.ServiceFQDN()
		if fqdn == "" {
			if hasCreds {
				return "", nil, errors.New("sandbox has no Service to address (spec.service unset on the template)")
			}
			return "", nil, ErrRunGoUnsupported
		}
		return "http://" + net.JoinHostPort(fqdn, strconv.Itoa(serverPort)), nil, nil
	}
	if s.router == nil || s.box == nil {
		return "", nil, ErrRunGoUnsupported
	}
	tgt := s.router.target.Load()
	if tgt == nil {
		probeCtx := context.WithValue(ctx, routerProbeKey{}, true)
		if _, err := s.box.Exists(probeCtx, "go.mod"); err != nil {
			return "", nil, fmt.Errorf("sandbox: probe router: %w", classifySessionErr(err))
		}
		tgt = s.router.target.Load()
		if tgt == nil {
			return "", nil, ErrRunGoUnsupported
		}
	}
	return tgt.baseURL, tgt, nil
}

type runGoWireRequest struct {
	Reset          bool              `json:"reset,omitempty"`
	PreserveDirs   []string          `json:"preserve_dirs,omitempty"`
	Files          map[string][]byte `json:"files,omitempty"`
	BuildCommand   string            `json:"build_command"`
	RunCommand     string            `json:"run_command"`
	Env            map[string]string `json:"env,omitempty"`
	Credentials    *Credentials      `json:"credentials,omitempty"`
	ResultPath     string            `json:"result_path,omitempty"`
	MaxResultBytes int64             `json:"max_result_bytes,omitempty"`
	PhaseTimeoutMS int64             `json:"phase_timeout_ms,omitempty"`
}

type runGoWireResponse struct {
	Phase          string `json:"phase"`
	ExitCode       int    `json:"exit_code"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	BuildStdout    string `json:"build_stdout"`
	BuildMS        int64  `json:"build_ms"`
	RunMS          int64  `json:"run_ms"`
	TimedOut       bool   `json:"timed_out"`
	Result         string `json:"result"`
	ResultSize     int64  `json:"result_size"`
	ResultReadErr  string `json:"result_read_err"`
	HasAttachments bool   `json:"has_attachments"`
}

// postRunGo sends one POST /run-go request without retries (runs are
// non-idempotent) and never echoes the request body or credentials in
// returned errors.
func postRunGo(ctx context.Context, hc *http.Client, baseURL string, router *routerTarget, r RunGoRequest, phaseTimeout time.Duration) (*runGoWireResponse, error) {
	payload, err := json.Marshal(runGoWireRequest{
		Reset:          r.Reset,
		PreserveDirs:   r.PreserveDirs,
		Files:          r.Files,
		BuildCommand:   r.BuildCommand,
		RunCommand:     r.RunCommand,
		Env:            r.Env,
		Credentials:    r.Credentials,
		ResultPath:     r.ResultPath,
		MaxResultBytes: r.MaxResultBytes,
		PhaseTimeoutMS: phaseTimeout.Milliseconds(),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/run-go", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if router != nil {
		req.Header.Set("X-Sandbox-ID", router.sandboxID)
		req.Header.Set("X-Sandbox-Namespace", router.namespace)
		port := router.port
		if port == "" {
			port = strconv.Itoa(serverPort)
		}
		req.Header.Set("X-Sandbox-Port", port)
		if deadline, ok := ctx.Deadline(); ok {
			if rem := time.Until(deadline); rem > 0 {
				req.Header.Set("X-Sandbox-Timeout", strconv.FormatFloat(rem.Seconds(), 'f', -1, 64))
			}
		}
	}

	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			return nil, fmt.Errorf("%w: %w", sb.ErrRetriesExhausted, err)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, ErrRunGoUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		httpErr := &sb.HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "run-go"}
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
			return nil, fmt.Errorf("%w: %w", sb.ErrRetriesExhausted, httpErr)
		}
		return nil, httpErr
	}

	var out runGoWireResponse
	lr := io.LimitedReader{R: resp.Body, N: maxRunGoResponse}
	if err := json.NewDecoder(&lr).Decode(&out); err != nil {
		if lr.N <= 0 {
			return nil, fmt.Errorf("%w: run-go output too large", sb.ErrResponseTooLarge)
		}
		return nil, fmt.Errorf("decode run-go result: %w", err)
	}
	return &out, nil
}
