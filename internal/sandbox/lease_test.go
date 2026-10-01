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
	"errors"
	"sync"
	"testing"
	"time"
)

type fakePatcher struct {
	mu    sync.Mutex
	times []time.Time
	err   error
}

func (f *fakePatcher) patchLifecycle(_ context.Context, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.times = append(f.times, t)
	return nil
}

func (f *fakePatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.times)
}

func TestLeasePatch(t *testing.T) {
	at := time.Date(2026, 9, 30, 15, 4, 5, 0, time.FixedZone("EDT", -4*3600))
	got, err := leasePatch(at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"spec":{"lifecycle":{"shutdownPolicy":"Delete","shutdownTime":"2026-09-30T19:04:05Z"}}}`
	if string(got) != want {
		t.Errorf("patch = %s, want %s", got, want)
	}
}

func TestLeaseRenewsUntilStopped(t *testing.T) {
	f := &fakePatcher{}
	start := time.Now()
	l, err := startLease(context.Background(), f, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("patches after start = %d, want 1 (the first is synchronous)", f.count())
	}
	f.mu.Lock()
	first := f.times[0]
	f.mu.Unlock()
	if first.Before(start.Add(30*time.Millisecond)) || first.After(time.Now().Add(30*time.Millisecond)) {
		t.Errorf("first shutdownTime %v isn't now+period", first)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.count() < 3 {
		t.Fatalf("patches = %d after 2s, want renewals", f.count())
	}
	l.Stop()
	n := f.count()
	time.Sleep(50 * time.Millisecond)
	if f.count() != n {
		t.Errorf("renewed after Stop: %d -> %d", n, f.count())
	}
	l.Stop() // idempotent
}

func TestLeaseStartFails(t *testing.T) {
	f := &fakePatcher{err: errors.New("forbidden")}
	if _, err := startLease(context.Background(), f, time.Minute); err == nil {
		t.Fatal("startLease succeeded with a failing patcher")
	}
}
