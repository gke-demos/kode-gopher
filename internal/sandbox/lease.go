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
	"fmt"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var claimGVR = schema.GroupVersionResource{
	Group:    "extensions.agents.x-k8s.io",
	Version:  "v1beta1",
	Resource: "sandboxclaims",
}

// claimPatcher sets a claim's spec.lifecycle. The agent-sandbox client
// creates claims without one.
type claimPatcher interface {
	patchLifecycle(ctx context.Context, shutdownTime time.Time) error
}

type dynamicPatcher struct {
	client    dynamic.Interface
	namespace string
	name      string
}

func newDynamicPatcher(rc *rest.Config, namespace, name string) (*dynamicPatcher, error) {
	c, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &dynamicPatcher{client: c, namespace: namespace, name: name}, nil
}

func (p *dynamicPatcher) patchLifecycle(ctx context.Context, shutdownTime time.Time) error {
	body, err := leasePatch(shutdownTime)
	if err != nil {
		return err
	}
	_, err = p.client.Resource(claimGVR).Namespace(p.namespace).
		Patch(ctx, p.name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// leasePatch is the merge patch that (re)sets the claim's expiry. On
// expiry the controller deletes the claim, and with it the sandbox.
func leasePatch(shutdownTime time.Time) ([]byte, error) {
	return json.Marshal(map[string]any{
		"spec": map[string]any{
			"lifecycle": map[string]any{
				"shutdownTime":   shutdownTime.UTC().Format(time.RFC3339),
				"shutdownPolicy": "Delete",
			},
		},
	})
}

// lease keeps a claim's shutdownTime about one lease period ahead
// while the Session is open. If kode-gopher dies, the claim expires
// within the lease and the controller cleans up the sandbox.
type lease struct {
	patcher claimPatcher
	period  time.Duration
	now     func() time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// startLease sets the first expiry synchronously (so Open fails if the
// claim can't be leased), then renews every period/3 in the background.
func startLease(ctx context.Context, p claimPatcher, period time.Duration) (*lease, error) {
	l := &lease{patcher: p, period: period, now: time.Now, stop: make(chan struct{}), done: make(chan struct{})}
	if err := l.renew(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: set claim lease: %w", err)
	}
	// #nosec G118 -- renewal deliberately outlives ctx (Open's): it runs
	// until the session's Close or Disconnect calls Stop.
	go l.loop()
	return l, nil
}

func (l *lease) renew(ctx context.Context) error {
	return l.patcher.patchLifecycle(ctx, l.now().Add(l.period))
}

func (l *lease) loop() {
	defer close(l.done)
	t := time.NewTicker(l.period / 3)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), l.period/3)
			if err := l.renew(ctx); err != nil {
				// Keep trying: two more ticks before the lease runs out.
				log.Printf("sandbox: renew claim lease: %v", err)
			}
			cancel()
		}
	}
}

// Stop ends renewal. The claim keeps its last expiry, so a disconnected
// session's sandbox still goes away within one lease.
func (l *lease) Stop() {
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
}
