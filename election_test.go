package ronin

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

var nextTestPort uint32 = 20000

// getFreePorts finds n free ports on localhost.
func getFreePorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for len(ports) < n {
		p := int(atomic.AddUint32(&nextTestPort, 1))
		if p > 60000 {
			// Reset and keep searching.
			atomic.StoreUint32(&nextTestPort, 20000)
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = l.Close()
		ports = append(ports, p)
	}
	return ports
}

// waitForCondition polls until the condition is met or timeout occurs.
func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// setupClusterNode starts a single node manager.
// If clusterPort is negative, clustering is disabled (standalone mode).
func setupClusterNode(t *testing.T, id string, clientPort, clusterPort int, peers string) *Manager {
	t.Helper()

	natsOptions := func(o *server.Options) {
		o.NoLog = true
		o.NoSigs = true
		if clusterPort < 0 {
			o.Cluster = server.ClusterOpts{}
		}
	}

	mgr, err := NewManager(ServerConfig{
		NodeID:      id,
		Host:        "127.0.0.1",
		ClientPort:  clientPort,
		ClusterPort: clusterPort,
		Peers:       peers,
		DataDir:     t.TempDir(),
		NATSOptions: natsOptions,
		Logging:     &ServeLog{},
	})
	if err != nil {
		t.Fatalf("[%s] failed to create manager: %v", id, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("[%s] failed to start manager: %v", id, err)
	}

	// Register cleanup to ensure shutdown
	t.Cleanup(func() {
		mgr.Shutdown()
	})

	return mgr
}

// TestSingleNodeElection verifies that a single node can become a leader.
func TestSingleNodeElection(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-1", ports[0], -1, "")

	// Create Candidate
	var isLeader atomic.Bool
	candidate, err := mgr.NewCandidate(CandidateConfig{
		CandidateID: "cand-1",
		BucketName:  "test_bucket",
		KeyName:     "test_key",
		TTL:         2 * time.Second,
		// Callbacks run from the election goroutine; keep them sync-safe.
		SynchronousCallbacks: true,
		OnElected: func() {
			isLeader.Store(true)
		},
		OnSteppingDown: func() {
			isLeader.Store(false)
		},
	})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	// Run in background
	ctx := t.Context()

	go func() {
		candidate.Run(ctx)
	}()

	// Verify leadership acquisition
	waitForCondition(t, 5*time.Second, func() bool {
		return candidate.IsLeader() && isLeader.Load()
	}, "candidate to become leader")

	if candidate.CurrentLeader() != "cand-1" {
		t.Errorf("expected leader 'cand-1', got '%s'", candidate.CurrentLeader())
	}
}

// TestBucketConfigConsistency verifies that the Manager enforces consistent config.
func TestBucketConfigConsistency(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-config", ports[0], -1, "")

	// 1. Create first candidate (defines the bucket config)
	_, err := mgr.NewCandidate(CandidateConfig{
		CandidateID: "c1",
		BucketName:  "shared_bucket",
		KeyName:     "key1",
		TTL:         5 * time.Second,
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("failed to create first candidate: %v", err)
	}

	// 2. Create second candidate with SAME config (Should succeed)
	_, err = mgr.NewCandidate(CandidateConfig{
		CandidateID: "c2",
		BucketName:  "shared_bucket",
		KeyName:     "key2",
		TTL:         5 * time.Second,
		Replicas:    1,
	})
	if err != nil {
		t.Errorf("failed to create compatible candidate: %v", err)
	}

	// 3. Create third candidate with DIFFERENT TTL (Should fail)
	_, err = mgr.NewCandidate(CandidateConfig{
		CandidateID: "c3",
		BucketName:  "shared_bucket",
		KeyName:     "key3",
		TTL:         10 * time.Second, // Mismatch!
		Replicas:    1,
	})
	if err == nil {
		t.Error("expected error for TTL mismatch, got nil")
	}

	// 4. Create fourth candidate with DIFFERENT Replicas (Should fail)
	_, err = mgr.NewCandidate(CandidateConfig{
		CandidateID: "c4",
		BucketName:  "shared_bucket",
		KeyName:     "key4",
		TTL:         5 * time.Second,
		Replicas:    3, // Mismatch!
	})
	if err == nil {
		t.Error("expected error for Replicas mismatch, got nil")
	}
}

// TestMultiNodeClusterElection verifies leader election and failover in a 3-node cluster.
func TestMultiNodeClusterElection(t *testing.T) {
	t.Parallel()
	// Setup 3 nodes
	nodeCount := 3
	// We need 2 ports per node (Client + Cluster)
	ports := getFreePorts(t, nodeCount*2)

	// Build peer string
	var peersStr string
	for i := range nodeCount {
		if i > 0 {
			peersStr += ","
		}
		peersStr += fmt.Sprintf("nats://127.0.0.1:%d", ports[i*2+1])
	}

	managers := make([]*Manager, nodeCount)
	candidates := make([]*Candidate, nodeCount)
	ctxs := make([]context.Context, nodeCount)
	cancels := make([]context.CancelFunc, nodeCount)

	// Start Cluster
	for i := range nodeCount {
		id := fmt.Sprintf("node-%d", i)
		managers[i] = setupClusterNode(t, id, ports[i*2], ports[i*2+1], peersStr)

		// Create Candidate on each node
		var err error
		candidates[i], err = managers[i].NewCandidate(CandidateConfig{
			CandidateID: id,
			BucketName:  "cluster_election",
			KeyName:     "leader",
			TTL:         3 * time.Second, // Short TTL for fast failover
			Replicas:    3,               // Quorum required
		})
		if err != nil {
			t.Fatalf("failed to create candidate %d: %v", i, err)
		}

		ctxs[i], cancels[i] = context.WithCancel(context.Background())

		// Run election loop
		go func(idx int) {
			candidates[idx].Run(ctxs[idx])
		}(i)
	}

	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	// 1. Wait for a leader to be elected
	var initialLeaderID string
	waitForCondition(t, 30*time.Second, func() bool {
		leaders := 0
		for _, c := range candidates {
			if c.IsLeader() {
				leaders++
				initialLeaderID = c.CurrentLeader()
			}
		}
		return leaders == 1
	}, "exactly one leader to be elected")

	t.Logf("Initial Leader elected: %s", initialLeaderID)

	// 2. Identify the leader index
	leaderIdx := -1
	for i, c := range candidates {
		if c.cfg.CandidateID == initialLeaderID {
			leaderIdx = i
			break
		}
	}

	// 3. KILL the leader (Cancel context -> Stop Candidate -> Stop Manager)
	t.Logf("Stopping leader: %s", initialLeaderID)
	cancels[leaderIdx]() // Stop candidate loop (Resign attempt)

	// Wait a bit to ensure resignation/shutdown propagates
	time.Sleep(500 * time.Millisecond)
	managers[leaderIdx].Shutdown() // Stop NATS server physically

	// 4. Wait for RE-ELECTION
	// Since the leader resigned gracefully (or NATS stopped), a new leader should be chosen.
	var newLeaderID string
	waitForCondition(t, 30*time.Second, func() bool {
		for i, c := range candidates {
			if i == leaderIdx {
				continue // Skip the dead node
			}
			if c.IsLeader() {
				newLeaderID = c.CurrentLeader()
				return true
			}
		}
		return false
	}, "new leader to be elected after failover")

	if newLeaderID == initialLeaderID {
		t.Fatalf("Leader did not change! Still %s", newLeaderID)
	}

	t.Logf("Failover successful! New Leader: %s", newLeaderID)
}
