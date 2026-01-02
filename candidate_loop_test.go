package ronin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCandidateRunElectionLoopRenewsAndResignsOnCancel(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-loop-renew", ports[0], -1, "")

	cand, err := mgr.NewCandidate(CandidateConfig{
		CandidateID:          "cand",
		BucketName:           "b-loop-renew",
		KeyName:              "k-loop-renew",
		TTL:                  800 * time.Millisecond,
		RenewInterval:        100 * time.Millisecond,
		SynchronousCallbacks: true,
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	rev, err := cand.kv.Create(ctx, cand.cfg.KeyName, []byte(cand.cfg.CandidateID))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	cand.becomeLeader(rev)
	oldRev := cand.lastRev

	done := make(chan error, 1)
	go func() {
		done <- cand.runElectionLoop(ctx)
	}()

	// Wait for at least one renew tick to advance the revision.
	waitForCondition(t, 5*time.Second, func() bool {
		cand.mu.RLock()
		defer cand.mu.RUnlock()
		return cand.lastRev > oldRev
	}, "revision to advance via renew")

	cancel()
	select {
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for election loop to stop")
	case <-done:
	}
}

func TestCandidateRunElectionLoopRetriesAcquireWhenKeyExists(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-loop-retry", ports[0], -1, "")

	cand, err := mgr.NewCandidate(CandidateConfig{
		CandidateID:          "cand",
		BucketName:           "b-loop-retry",
		KeyName:              "k-loop-retry",
		TTL:                  1 * time.Second,
		RenewInterval:        100 * time.Millisecond,
		SynchronousCallbacks: true,
		Observer:             false,
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	// Block acquisition by pre-creating the key owned by someone else.
	if _, err := cand.kv.Create(ctx, cand.cfg.KeyName, []byte("other")); err != nil {
		t.Fatalf("failed to create blocking key: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cand.runElectionLoop(ctx)
	}()

	// Give it a couple of ticks; it should still not become leader.
	time.Sleep(350 * time.Millisecond)
	if cand.IsLeader() {
		t.Fatalf("expected not to become leader while key exists")
	}

	cancel()
	select {
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for election loop to stop")
	case <-done:
	}
}

func TestCandidateRunElectionLoopCampaignTriggersAcquire(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-loop-campaign", ports[0], -1, "")

	var elected atomic.Bool
	cand, err := mgr.NewCandidate(CandidateConfig{
		CandidateID:          "cand",
		BucketName:           "b-loop-campaign",
		KeyName:              "k-loop-campaign",
		TTL:                  1 * time.Second,
		RenewInterval:        200 * time.Millisecond,
		SynchronousCallbacks: true,
		OnElected:            func() { elected.Store(true) },
		Observer:             false,
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	// Start with a blocking leader.
	if _, err := cand.kv.Create(ctx, cand.cfg.KeyName, []byte("other")); err != nil {
		t.Fatalf("failed to create blocking key: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cand.runElectionLoop(ctx)
	}()

	// Now delete the key and poke campaign channel.
	if err := cand.kv.Delete(ctx, cand.cfg.KeyName); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	select {
	case cand.campaignCh <- struct{}{}:
	default:
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return cand.IsLeader() && elected.Load()
	}, "candidate to acquire leadership after campaign")

	cancel()
	select {
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for election loop to stop")
	case <-done:
	}
}
