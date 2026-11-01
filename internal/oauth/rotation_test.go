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

package oauth

// Tests for rotation IDs: a refresh that gives its rotation back must
// never give back another refresh's, or the client's retry mints a second
// live branch of the family. Ported from go-steer/steward#22.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// faultyReplay stands in for a shared replay cache over a network: its
// client can deliver a rotation twice (a retry after a lost reply), and
// a give-back can be delivered again later (a duplicate that lands after
// the client's retry). It wraps the server's in-memory cache.
type faultyReplay struct {
	*replayCache

	mu    sync.Mutex
	twice int      // deliver this many rotations twice
	hold  int      // keep this many give-backs to deliver again
	held  []func() // the kept give-backs
}

func newFaultyReplay(e *testEnv) *faultyReplay {
	c := &faultyReplay{replayCache: e.as.used.(*replayCache)}
	e.as.used = c
	return c
}

func (c *faultyReplay) rotate(fam string, gen uint64, exp time.Time, rot string) bool {
	ok := c.replayCache.rotate(fam, gen, exp, rot)
	c.mu.Lock()
	again := c.twice > 0
	if again {
		c.twice--
	}
	c.mu.Unlock()
	if again {
		ok = c.replayCache.rotate(fam, gen, exp, rot)
	}
	return ok
}

func (c *faultyReplay) unrotate(fam string, gen uint64, rot string) {
	c.replayCache.unrotate(fam, gen, rot)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hold > 0 {
		c.hold--
		c.held = append(c.held, func() { c.replayCache.unrotate(fam, gen, rot) })
	}
}

// redeliver delivers the kept give-backs again.
func (c *faultyReplay) redeliver() {
	c.mu.Lock()
	held := c.held
	c.held = nil
	c.mu.Unlock()
	for _, f := range held {
		f()
	}
}

// The regression, adapted from steward's two replicas sharing a cache: a
// refresh that failed for a transient reason gives its rotation back, and
// the client's retry rotates the family again. If the give-back is
// delivered again after that (a networked cache's duplicate), it must not
// undo the retry's rotation, or presenting the first token once more
// rotates the family a third time: the rotated token has yielded tokens
// twice, and two refresh tokens of the next generation are live.
func TestGiveBackDeliveredTwice(t *testing.T) {
	e := newTestEnv(t, true, nil)
	c := newFaultyReplay(e)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	tokenURL := e.as.google.client.TokenURL
	e.as.google.client.TokenURL = down.URL
	c.mu.Lock()
	c.hold = 1
	c.mu.Unlock()
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "server_error" {
		t.Fatalf("refresh while Google is down: %+v, want server_error", r)
	}
	e.as.google.client.TokenURL = tokenURL
	retry := e.token(refreshForm(id, first.Refresh), "", "")
	if retry.status != http.StatusOK {
		t.Fatalf("the client's retry: %+v", retry)
	}

	c.redeliver() // the failed refresh's give-back, again
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.status == http.StatusOK || r.Error != "invalid_grant" {
		t.Fatalf("the rotated token once more: status %d %q, want invalid_grant (a second branch of the family)", r.status, r.Error)
	}
	if r := e.token(refreshForm(id, retry.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("the retry's refresh token after the replay: %+v, want invalid_grant (family revoked)", r)
	}
}

// A cache client that delivers a rotation twice (retrying after a reply
// it lost) must not turn the refresh into a replay and sign the person
// out: rotate is idempotent for its own ID.
func TestRotateRedelivered(t *testing.T) {
	e := newTestEnv(t, true, nil)
	c := newFaultyReplay(e)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	c.mu.Lock()
	c.twice = 1
	c.mu.Unlock()
	second := e.token(refreshForm(id, first.Refresh), "", "")
	if second.status != http.StatusOK {
		t.Fatalf("refresh whose rotation was delivered twice: %+v", second)
	}
	if r := e.token(refreshForm(id, second.Refresh), "", ""); r.status != http.StatusOK {
		t.Errorf("its refresh token: %+v (the family was revoked)", r)
	}
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("the first token after its rotation applied: %+v, want invalid_grant", r)
	}
}

// gateGroups holds the first group check until released, then fails it
// with the error released; later checks answer from members at once.
type gateGroups struct {
	entered chan struct{}
	release chan error
	members *fakeGroups
	gated   atomic.Bool
}

func (g *gateGroups) IsMember(ctx context.Context, group, member string) (bool, error) {
	if g.gated.CompareAndSwap(false, true) {
		close(g.entered)
		if err := <-g.release; err != nil {
			return false, err
		}
	}
	return g.members.IsMember(ctx, group, member)
}

// In memory, a give-back can't follow a lost race: a refresh whose
// rotation applied and that then fails for a transient reason, after a
// concurrent refresh with the same token was refused as a replay, gives
// back nothing, and the client's retry is refused too. (The concurrent
// refresh revoked the family.) This passes with or without rotation IDs;
// it pins what the in-memory cache guarantees.
func TestTransientFailureAfterConcurrentReuse(t *testing.T) {
	e := newTestEnv(t, true, nil)
	e.google.signInAs = bob // admitted by group, so the gate is consulted
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")

	gate := &gateGroups{entered: make(chan struct{}), release: make(chan error, 1), members: e.groups}
	// A failed assertion must not leave the first refresh held at the
	// gate, or closing the test server waits on it for ever.
	t.Cleanup(func() {
		select {
		case gate.release <- errors.New("test over"):
		default:
		}
	})
	e.as.cfg.Allow.GroupChecker = gate
	e.as.cfg.Allow.cache = nil
	held := make(chan tokenResp, 1)
	go func() { held <- e.token(refreshForm(id, first.Refresh), "", "") }()
	select {
	case <-gate.entered: // it has rotated the family
	case <-time.After(10 * time.Second):
		t.Fatal("the first refresh never reached the group check")
	}
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Fatalf("a concurrent refresh with the same token: %+v, want invalid_grant", r)
	}
	gate.release <- errors.New("directory unavailable")
	if r := <-held; r.Error != "server_error" {
		t.Fatalf("the first refresh, failing for a transient reason: %+v, want server_error", r)
	}
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.status == http.StatusOK || r.Error != "invalid_grant" {
		t.Errorf("the client's retry after the reuse: status %d %q, want invalid_grant", r.status, r.Error)
	}
}

func TestReplayCacheRotationID(t *testing.T) {
	c := &replayCache{seen: map[string]time.Time{}}
	exp := time.Now().Add(time.Hour)
	at := func(fam string) family {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.families[fam]
	}

	// Only the rotation's own ID gives it back, once.
	if !c.rotate("a", 0, exp, "a1") || at("a").gen != 1 || at("a").rot != "a1" {
		t.Fatalf("first rotation: %+v", at("a"))
	}
	// The same rotation delivered again is not a replay.
	if !c.rotate("a", 0, exp, "a1") || at("a").gen != 1 || at("a").revoked {
		t.Errorf("the same rotation again: %+v, want applied, unchanged", at("a"))
	}
	for _, id := range []string{"other", ""} {
		c.unrotate("a", 0, id)
		if at("a").gen != 1 {
			t.Errorf("unrotate(%q) gave back rotation a1", id)
		}
	}
	c.unrotate("a", 5, "a1") // the right ID, the wrong generation
	if at("a").gen != 1 {
		t.Error("unrotate gave back a rotation named for another generation")
	}
	c.unrotate("a", 0, "a1")
	if f := at("a"); f.gen != 0 || f.rot != "" {
		t.Errorf("unrotate(a1): %+v, want generation 0 and no ID", f)
	}
	if !c.rotate("a", 0, exp, "a2") {
		t.Error("rotation after a give-back refused")
	}
	c.unrotate("a", 0, "a1") // a1 was given back already
	if at("a").gen != 1 {
		t.Error("a given-back rotation's ID gave back the next rotation")
	}

	// The family moved on: neither an older rotation's ID nor, once the
	// newer rotation is given back, the older ID again moves it.
	if !c.rotate("a", 1, exp, "a3") {
		t.Fatal("rotation to generation 2 refused")
	}
	c.unrotate("a", 0, "a2")
	c.unrotate("a", 1, "a2")
	if at("a").gen != 2 {
		t.Errorf("an older rotation's ID moved the family: %+v", at("a"))
	}
	c.unrotate("a", 1, "a3")
	c.unrotate("a", 0, "a2") // the family is at 1 again, but not by a2's rotation as far as it knows
	if f := at("a"); f.gen != 1 {
		t.Errorf("after giving back a3 and trying a2: %+v, want generation 1", f)
	}

	// An earlier generation's rotation delivered again, once the family
	// has moved past it, is a replay.
	if !c.rotate("d", 0, exp, "d1") || !c.rotate("d", 1, exp, "d2") {
		t.Fatal("rotating family d refused")
	}
	if c.rotate("d", 0, exp, "d1") || !at("d").revoked {
		t.Errorf("an older rotation delivered after the family moved on: %+v, want revoked", at("d"))
	}

	// A revoked family stays revoked; the reuse that revoked it, and the
	// rotation before, give nothing back.
	if !c.rotate("r", 0, exp, "r1") {
		t.Fatal("rotating family r refused")
	}
	if c.rotate("r", 0, exp, "r2") {
		t.Fatal("reuse accepted")
	}
	c.unrotate("r", 0, "r1")
	c.unrotate("r", 0, "r2")
	if f := at("r"); !f.revoked {
		t.Errorf("unrotate revived a revoked family: %+v", f)
	}
	if c.rotate("r", 0, exp, "r1") {
		t.Error("a revoked family's rotation delivered again was accepted")
	}

	// A rotation without an ID records none, so nothing gives it back,
	// and a rotation with none is never taken for a redelivery.
	if !c.rotate("p", 0, exp, "") {
		t.Fatal("rotating family p refused")
	}
	c.unrotate("p", 0, "")
	if at("p").gen != 1 {
		t.Error("an empty ID gave back a rotation without one")
	}
	if c.rotate("p", 0, exp, "") {
		t.Error("a rotation without an ID, presented again, was taken for a redelivery")
	}

	// A later generation than the family's moves it on and records the ID;
	// so does an unknown family.
	if !c.rotate("q", 0, exp, "q1") || !c.rotate("q", 7, exp, "q8") || at("q").gen != 8 || at("q").rot != "q8" {
		t.Errorf("rotation from a later generation: %+v", at("q"))
	}
	if !c.rotate("u", 4, exp, "u5") || at("u").gen != 5 || at("u").rot != "u5" {
		t.Errorf("rotation of an unknown family: %+v", at("u"))
	}
}

// One refresh rotates a generation while many give-backs, each with its
// own ID (from a refresh whose rotation never applied), land at the same
// time: exactly the rotation stands, every round.
func TestReplayCacheRotationIDConcurrent(t *testing.T) {
	c := &replayCache{seen: map[string]time.Time{}}
	exp := time.Now().Add(time.Hour)
	const rounds, n = 200, 8
	for gen := range uint64(rounds) {
		var wg sync.WaitGroup
		var applied atomic.Int32
		start := make(chan struct{})
		winner := fmt.Sprintf("%d-w", gen)
		wg.Go(func() {
			<-start
			if c.rotate("f", gen, exp, winner) {
				applied.Add(1)
			}
		})
		for i := range n {
			wg.Go(func() {
				<-start
				c.unrotate("f", gen, fmt.Sprintf("%d-%d", gen, i))
			})
		}
		close(start)
		wg.Wait()
		c.mu.Lock()
		f := c.families["f"]
		c.mu.Unlock()
		if applied.Load() != 1 || f.gen != gen+1 || f.revoked || f.rot != winner {
			t.Fatalf("generation %d: applied %d, family %+v after the give-backs, want generation %d by %s", gen, applied.Load(), f, gen+1, winner)
		}
	}
}
