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
	"errors"
	"strings"
)

// ErrSessionDead is wrapped around Execute errors we've classified as
// unrecoverable within the current Session — the pod is gone, the
// port-forward closed, or the upstream retry loop gave up. Callers can
// test with errors.Is and reopen a fresh session to recover.
//
// The classification is done exactly once, in classifySessionErr,
// against a hand-curated corpus of upstream error phrases (see
// errors_test.go). The upstream client emits opaque wrapped strings
// with no typed sub-errors we can key off, so pattern-matching is the
// only signal available — kept in this one place so future upstream
// message tweaks fail LOUD in tests rather than silently disabling
// the retry path.
var ErrSessionDead = errors.New("sandbox: session no longer usable, reopen required")

// classifySessionErr wraps err with ErrSessionDead if its Error()
// contains one of the known dead-session phrases. Returns err
// unchanged otherwise. Nil input returns nil.
func classifySessionErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, phrase := range deadSessionPhrases {
		if strings.Contains(msg, phrase) {
			return &sessionDeadError{orig: err}
		}
	}
	return err
}

// deadSessionPhrases is the classification corpus. Add cautiously —
// each entry is a claim that "if you see this string in an Execute
// error, the session is unrecoverable and worth reopening." The
// corpus test enforces every phrase actually appears in a realistic
// error, so drift doesn't silently expand the retry surface.
var deadSessionPhrases = []string{
	// Upstream agent-sandbox HTTP retry loop exhausted its attempts.
	// Observed at the fresh-pod boundary and when the sandbox-router
	// port-forward drops mid-request.
	"retries exhausted",
	// Sandbox claim was deleted out from under us (controller sweep,
	// operator kubectl delete, warmpool eviction).
	"sandbox not found",
	// Port-forward TCP-refused after the pod restarted.
	"connection refused",
	// Response-headers HTTP timeout (the PerAttemptTimeout budget
	// exhausted with the server still buffering). Not always fatal
	// but a fresh session is the right move — the current one is
	// wedged on the previous exec.
	"timeout awaiting response headers",
	// gVisor / kubelet killed the pod (OOMKilled, gVisor sandbox
	// crash). Upstream surfaces the k8s "connection reset" as
	// ECONNRESET-shaped errors.
	"connection reset by peer",
}

// sessionDeadError makes errors.Is(err, ErrSessionDead) return true
// while keeping the original error's text and unwrap chain intact —
// so callers wrapping with fmt.Errorf still produce readable messages
// without our sentinel prefix showing up multiple times in the chain.
type sessionDeadError struct{ orig error }

func (e *sessionDeadError) Error() string          { return e.orig.Error() }
func (e *sessionDeadError) Is(target error) bool   { return target == ErrSessionDead }
func (e *sessionDeadError) Unwrap() error          { return e.orig }
// Cause is a common helper convention for extracting the wrapped error.
func (e *sessionDeadError) Cause() error { return e.orig }
