package ronin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestNewCandidateDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	_, err := newCandidate(nil, CandidateConfig{})
	if err == nil {
		t.Fatalf("expected error for missing CandidateID")
	}

	c, err := newCandidate(nil, CandidateConfig{CandidateID: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c.cfg.BucketName == "" || c.cfg.KeyName == "" {
		t.Fatalf("expected defaults for bucket/key")
	}
	if c.cfg.TTL == 0 || c.cfg.RenewInterval == 0 {
		t.Fatalf("expected defaults for ttl/renew interval")
	}
	if c.cfg.Replicas == 0 {
		t.Fatalf("expected default replicas")
	}
}

func TestCandidateLeaderRevStalenessAndStepDown(t *testing.T) {
	t.Parallel()
	steppedDown := 0

	c, err := newCandidate(nil, CandidateConfig{
		CandidateID:          "c",
		TTL:                  200 * time.Millisecond,
		SynchronousCallbacks: true,
		OnSteppingDown:       func() { steppedDown++ },
		OnLeaderChange:       func(string) {},
		OnRenewFailure:       func(error) {},
		OnElected:            func() {},
		MeterProvider:        nil,
		Observer:             true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c.mu.Lock()
	c.isLeader = true
	c.lastRev = 123
	c.lastRenewTime = time.Now()
	c.mu.Unlock()

	if got := c.LeaderRev(); got != 123 {
		t.Fatalf("expected leader rev 123, got %d", got)
	}

	c.mu.Lock()
	c.isLeader = false
	c.mu.Unlock()
	if got := c.LeaderRev(); got != 0 {
		t.Fatalf("expected leader rev 0 when not leader, got %d", got)
	}

	c.mu.Lock()
	c.isLeader = true
	c.lastRev = 123
	c.lastRenewTime = time.Now().Add(-c.cfg.TTL) // triggers safety-margin stale
	c.mu.Unlock()
	if c.IsLeader() {
		t.Fatalf("expected IsLeader to be false when stale")
	}

	c.mu.Lock()
	c.lastRenewTime = time.Now().Add(-c.cfg.TTL * 2)
	c.mu.Unlock()
	if got := c.LeaderRev(); got != 0 {
		t.Fatalf("expected stale leader rev 0, got %d", got)
	}

	c.mu.Lock()
	c.isLeader = true
	c.leaderID = c.cfg.CandidateID
	c.lastRev = 1
	c.lastRenewTime = time.Now()
	c.mu.Unlock()

	c.stepDown()
	if c.IsLeader() {
		t.Fatalf("expected to step down")
	}
	if steppedDown != 1 {
		t.Fatalf("expected stepping down callback once, got %d", steppedDown)
	}

	// stepping down again as follower should not invoke callback
	c.stepDown()
	if steppedDown != 1 {
		t.Fatalf("expected stepping down callback still once, got %d", steppedDown)
	}
}

func TestCandidateInvokeSteppingDownNilCallback(t *testing.T) {
	t.Parallel()
	c, err := newCandidate(nil, CandidateConfig{CandidateID: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// should be a no-op
	c.invokeSteppingDown()
}

func TestCandidateInvokeCallbacksSyncAndAsync(t *testing.T) {
	t.Parallel()
	syncCalls := 0
	cSync, err := newCandidate(nil, CandidateConfig{
		CandidateID:          "c",
		SynchronousCallbacks: true,
		OnElected:            func() { syncCalls++ },
		OnSteppingDown:       func() { syncCalls++ },
		OnLeaderChange:       func(string) { syncCalls++ },
		OnRenewFailure:       func(error) { syncCalls++ },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cSync.invokeElected()
	cSync.invokeSteppingDown()
	cSync.invokeLeaderChange("x")
	cSync.invokeRenewFailure(errors.New("x"))
	if syncCalls != 4 {
		t.Fatalf("expected 4 synchronous calls, got %d", syncCalls)
	}

	ch := make(chan struct{}, 4)
	cAsync, err := newCandidate(nil, CandidateConfig{
		CandidateID:          "c",
		SynchronousCallbacks: false,
		OnElected:            func() { ch <- struct{}{} },
		OnSteppingDown:       func() { ch <- struct{}{} },
		OnLeaderChange:       func(string) { ch <- struct{}{} },
		OnRenewFailure:       func(error) { ch <- struct{}{} },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cAsync.invokeElected()
	cAsync.invokeSteppingDown()
	cAsync.invokeLeaderChange("x")
	cAsync.invokeRenewFailure(errors.New("x"))

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for i := 0; i < 4; i++ {
		select {
		case <-ch:
		case <-deadline.C:
			t.Fatalf("timeout waiting for async callback %d", i)
		}
	}
}

func TestCandidateRenewSuccessAndNotFoundStepsDown(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-renew", ports[0], -1, "")

	renewFailures := 0
	steppedDown := 0

	cand, err := mgr.NewCandidate(CandidateConfig{
		CandidateID:          "cand",
		BucketName:           "b-renew",
		KeyName:              "k-renew",
		TTL:                  500 * time.Millisecond,
		RenewInterval:        100 * time.Millisecond,
		SynchronousCallbacks: true,
		OnRenewFailure:       func(error) { renewFailures++ },
		OnSteppingDown:       func() { steppedDown++ },
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	rev, err := cand.kv.Create(ctx, cand.cfg.KeyName, []byte(cand.cfg.CandidateID))
	if err != nil {
		t.Fatalf("failed to create lease key: %v", err)
	}
	cand.becomeLeader(rev)
	oldRev := cand.lastRev

	cand.renew(ctx)
	if cand.lastRev == oldRev {
		t.Fatalf("expected revision to change after renew")
	}
	if !cand.IsLeader() {
		t.Fatalf("expected to remain leader after successful renew")
	}
	if renewFailures != 0 || steppedDown != 0 {
		t.Fatalf("expected no failures/stepdown on success (failures=%d, stepdowns=%d)", renewFailures, steppedDown)
	}

	if err := cand.kv.Delete(ctx, cand.cfg.KeyName); err != nil {
		t.Fatalf("failed to delete lease key: %v", err)
	}
	cand.renew(ctx)
	if cand.IsLeader() {
		t.Fatalf("expected to step down after renew failure")
	}
	if renewFailures == 0 {
		t.Fatalf("expected renew failure callback")
	}
	if steppedDown == 0 {
		t.Fatalf("expected stepping down callback")
	}
}

func TestCandidateRenewUnknownErrorStaleStepsDown(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-stale", ports[0], -1, "")

	steppedDown := 0
	cand, err := mgr.NewCandidate(CandidateConfig{
		CandidateID:          "cand",
		BucketName:           "b-stale",
		KeyName:              "k-stale",
		TTL:                  200 * time.Millisecond,
		RenewInterval:        100 * time.Millisecond,
		SynchronousCallbacks: true,
		OnSteppingDown:       func() { steppedDown++ },
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	rev, err := cand.kv.Create(ctx, cand.cfg.KeyName, []byte(cand.cfg.CandidateID))
	if err != nil {
		t.Fatalf("failed to create lease key: %v", err)
	}
	cand.becomeLeader(rev)

	cand.mu.Lock()
	cand.lastRenewTime = time.Now().Add(-cand.cfg.TTL * 2)
	cand.mu.Unlock()

	mgr.Conn().Close()
	cand.renew(ctx)

	if steppedDown == 0 {
		t.Fatalf("expected stepping down due to stale leader on non-KV error")
	}
	if cand.IsLeader() {
		t.Fatalf("expected not leader")
	}
}

func waitForKVEntry(t *testing.T, updates <-chan jetstream.KeyValueEntry, timeout time.Duration, wantOp jetstream.KeyValueOp) jetstream.KeyValueEntry {
	t.Helper()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		select {
		case <-deadline.C:
			t.Fatalf("timeout waiting for kv entry op=%v", wantOp)
		case e, ok := <-updates:
			if !ok {
				t.Fatalf("watcher updates channel closed")
			}
			if e == nil {
				continue
			}
			if e.Operation() == wantOp {
				return e
			}
		}
	}
}

func TestCandidateHandleWatcherEntryPutAndDelete(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-watch", ports[0], -1, "")

	var (
		steppingDownCalled bool
		leaderChangeTo     string
	)
	cand, err := mgr.NewCandidate(CandidateConfig{
		CandidateID:          "cand",
		BucketName:           "b-watch",
		KeyName:              "k-watch",
		TTL:                  2 * time.Second,
		SynchronousCallbacks: true,
		OnSteppingDown:       func() { steppingDownCalled = true },
		OnLeaderChange:       func(newLeader string) { leaderChangeTo = newLeader },
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	watcher, err := cand.kv.Watch(ctx, cand.cfg.KeyName)
	if err != nil {
		t.Fatalf("watch failed: %v", err)
	}
	defer watcher.Stop()

	cand.mu.Lock()
	cand.isLeader = true
	cand.leaderID = cand.cfg.CandidateID
	cand.lastRev = 1
	cand.lastRenewTime = time.Now()
	cand.mu.Unlock()

	// Simulate a new leader being put
	if _, err := cand.kv.Put(ctx, cand.cfg.KeyName, []byte("other")); err != nil {
		t.Fatalf("put failed: %v", err)
	}
	putEntry := waitForKVEntry(t, watcher.Updates(), 5*time.Second, jetstream.KeyValuePut)
	cand.handleWatcherEntry(putEntry)

	if cand.IsLeader() {
		t.Fatalf("expected to no longer be leader after observing other leader")
	}
	if !steppingDownCalled {
		t.Fatalf("expected stepping down callback")
	}
	if leaderChangeTo != "other" {
		t.Fatalf("expected leader change to 'other', got '%s'", leaderChangeTo)
	}

	// Simulate deletion that should trigger campaigning
	if err := cand.kv.Delete(ctx, cand.cfg.KeyName); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	delEntry := waitForKVEntry(t, watcher.Updates(), 5*time.Second, jetstream.KeyValueDelete)
	cand.handleWatcherEntry(delEntry)

	select {
	case <-cand.campaignCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected campaign signal")
	}
}
