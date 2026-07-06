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
	"fmt"
	"testing"
)

// TestClassifySessionErr_KnownDead is the classifier's corpus test:
// every entry is a realistic error string the upstream agent-sandbox
// client actually emits when a session is unrecoverable. If upstream
// changes its message wording, THIS test breaks and the
// deadSessionPhrases list needs updating — much better than silently
// losing the retry-once behavior in production.
func TestClassifySessionErr_KnownDead(t *testing.T) {
	corpus := []string{
		`sandbox: run: sandbox[default/sandbox-claim-mq2g6]: run failed: retries exhausted: POST failed after 1 attempts (url=http://127.0.0.1:40411/execute reqID=2f37e33b9754e145): Post "http://127.0.0.1:40411/execute": net/http: timeout awaiting response headers`,
		`sandbox: run: sandbox not found`,
		`sandbox: run: dial tcp 127.0.0.1:41903: connect: connection refused`,
		`sandbox: run: retries exhausted: unexpected EOF`,
		`sandbox: run: read tcp 10.0.0.1:8080->10.0.0.2:9090: read: connection reset by peer`,
	}
	for i, msg := range corpus {
		err := classifySessionErr(errors.New(msg))
		if !errors.Is(err, ErrSessionDead) {
			t.Errorf("corpus[%d] not classified as dead: %s", i, msg)
		}
	}
}

// TestClassifySessionErr_KeepsTransient asserts we DON'T classify
// non-fatal errors as dead sessions. A snippet build failure, a
// non-zero exit code, or a path-validation rejection should propagate
// as-is so the caller doesn't blow the pod away for a user bug.
func TestClassifySessionErr_KeepsTransient(t *testing.T) {
	transient := []string{
		`sandbox: write "main.go": permission denied`,
		`sandbox: tar extract failed (exit 2): tar: bad archive`,
		`sandbox: Request must set Command or Files`,
		`sandbox: invalid path: absolute paths not allowed`,
	}
	for i, msg := range transient {
		err := classifySessionErr(errors.New(msg))
		if errors.Is(err, ErrSessionDead) {
			t.Errorf("transient[%d] mis-classified as dead: %s", i, msg)
		}
	}
}

// TestClassifySessionErr_Nil confirms the classifier is safe with nil.
func TestClassifySessionErr_Nil(t *testing.T) {
	if err := classifySessionErr(nil); err != nil {
		t.Errorf("nil in should give nil out; got %v", err)
	}
}

// TestErrSessionDead_ChainSemantics confirms:
//   - Error() returns the original text verbatim (no sentinel prefix)
//   - errors.Is(err, ErrSessionDead) is true
//   - errors.Unwrap(err) returns the original for further traversal
//   - fmt.Errorf("...: %w", classified) still errors.Is(ErrSessionDead)
func TestErrSessionDead_ChainSemantics(t *testing.T) {
	orig := errors.New("retries exhausted: EOF")
	wrapped := classifySessionErr(orig)

	if wrapped.Error() != orig.Error() {
		t.Errorf("Error() = %q, want %q (original unchanged)", wrapped.Error(), orig.Error())
	}
	if !errors.Is(wrapped, ErrSessionDead) {
		t.Errorf("errors.Is(wrapped, ErrSessionDead) = false")
	}
	if got := errors.Unwrap(wrapped); got == nil || got.Error() != orig.Error() {
		t.Errorf("Unwrap = %v, want original", got)
	}

	// Outer wrap with caller context — errors.Is still finds sentinel.
	outer := fmt.Errorf("sandbox: run: %w", wrapped)
	if !errors.Is(outer, ErrSessionDead) {
		t.Errorf("errors.Is(outer, ErrSessionDead) = false through fmt.Errorf %%w chain")
	}
	if outer.Error() != "sandbox: run: "+orig.Error() {
		t.Errorf("outer.Error() = %q, expected clean prefix chain", outer.Error())
	}
}
