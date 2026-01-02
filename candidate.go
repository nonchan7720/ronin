package ronin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ======================================================================================
// Candidate: Application Layer
// Manages the leader election logic for a specific key (shard).
// ======================================================================================

// CandidateConfig defines the parameters for a specific election.
type CandidateConfig struct {
	// CandidateID is the unique identifier for this candidate instance (e.g., PodName).
	// In multi-shard scenarios, this typically stays the same across shards for the same process.
	CandidateID string

	// --- Lease Parameters ---
	BucketName string // KV Bucket name (default: "election_bucket")
	KeyName    string // Lease Key name (e.g. "shard_1")

	// TTL corresponds to "leaseDurationSeconds". (default: 5s)
	// NOTE: All Candidates using the same BucketName MUST have the same TTL.
	TTL time.Duration

	// RenewInterval corresponds to "renewDeadline". (default: TTL / 3)
	RenewInterval time.Duration

	// Replicas determines the replication factor of the KV bucket.
	// Critical for High Availability.
	// If 0, it defaults to 1. For production clusters, set this to 3.
	// NOTE: All Candidates using the same BucketName MUST have the same Replicas.
	Replicas int

	// --- Operational Modes ---
	Observer             bool
	SynchronousCallbacks bool

	// --- Observability ---
	// MeterProvider allows passing a custom OTel MeterProvider.
	// If nil, otel.GetMeterProvider() is used.
	MeterProvider metric.MeterProvider

	// --- Callbacks ---
	OnElected      func()
	OnSteppingDown func()
	OnLeaderChange func(newLeader string)

	// OnRenewFailure is called when the leader fails to renew the lease.
	// This can be used for metrics or logging.
	OnRenewFailure func(err error)
}

// Candidate represents a single participant in a leader election for a specific key.
type Candidate struct {
	cfg CandidateConfig
	nc  *nats.Conn
	js  jetstream.JetStream
	kv  jetstream.KeyValue

	mu            sync.RWMutex
	isLeader      bool
	leaderID      string
	lastRev       uint64
	lastRenewTime time.Time
	campaignCh    chan struct{}

	// Metrics
	metricReg     metric.Registration
	renewErrors   metric.Int64Counter
	leaderChanges metric.Int64Counter
}

// newCandidate creates a new Candidate using an existing NATS connection.
// This is internal to enforce usage of Manager.NewCandidate(), which handles
// bucket configuration consistency checks.
func newCandidate(nc *nats.Conn, cfg CandidateConfig) (*Candidate, error) {
	if cfg.CandidateID == "" {
		return nil, fmt.Errorf("CandidateConfig.CandidateID is required")
	}
	if cfg.BucketName == "" {
		cfg.BucketName = "election_bucket"
	}
	if cfg.KeyName == "" {
		cfg.KeyName = "current_leader"
	}
	if cfg.TTL == 0 {
		cfg.TTL = 5 * time.Second
	}
	if cfg.RenewInterval == 0 {
		cfg.RenewInterval = cfg.TTL / 3
	}
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}

	c := &Candidate{
		cfg:        cfg,
		nc:         nc,
		campaignCh: make(chan struct{}, 1),
	}

	// Initialize OpenTelemetry Metrics
	if err := c.initMetrics(); err != nil {
		return nil, fmt.Errorf("failed to init metrics: %w", err)
	}

	return c, nil
}

func (c *Candidate) initMetrics() error {
	mp := c.cfg.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter("github.com/your-org/election")

	// Attributes to identify this specific candidate instance
	baseAttrs := metric.WithAttributes(
		attribute.String("candidate_id", c.cfg.CandidateID),
		attribute.String("bucket", c.cfg.BucketName),
		attribute.String("key", c.cfg.KeyName),
	)

	// 1. Leader Status (Gauge): 1 if leader, 0 if not
	leaderStatus, err := meter.Int64ObservableGauge(
		"election.leader_status",
		metric.WithDescription("Current leadership status (1=Leader, 0=Follower)"),
	)
	if err != nil {
		return err
	}

	// 2. Leader Term/Revision (Gauge): Current revision number of the lease
	leaderTerm, err := meter.Int64ObservableGauge(
		"election.leader_term",
		metric.WithDescription("Current NATS KV revision number of the lease"),
	)
	if err != nil {
		return err
	}

	// 3. Renew Failures (Counter)
	c.renewErrors, err = meter.Int64Counter(
		"election.renew_failures_total",
		metric.WithDescription("Total number of lease renewal failures"),
	)
	if err != nil {
		return err
	}

	// 4. Leader Changes (Counter)
	c.leaderChanges, err = meter.Int64Counter(
		"election.leader_changes_total",
		metric.WithDescription("Total number of leadership changes observed"),
	)
	if err != nil {
		return err
	}

	// Register callback for Observable Gauges
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c.mu.RLock()
		defer c.mu.RUnlock()

		status := int64(0)
		// Use the same strict check as IsLeader()
		safetyMargin := c.cfg.TTL / 10
		if c.isLeader && time.Since(c.lastRenewTime) <= (c.cfg.TTL-safetyMargin) {
			status = 1
		}

		o.ObserveInt64(leaderStatus, status, baseAttrs)
		o.ObserveInt64(leaderTerm, int64(c.lastRev), baseAttrs)
		return nil
	}, leaderStatus, leaderTerm)

	if err != nil {
		return err
	}
	c.metricReg = reg
	return nil
}

// Run starts the election loop for this candidate.
func (c *Candidate) Run(ctx context.Context) error {
	// Ensure metrics are unregistered when Run finishes to avoid leaks
	defer func() {
		if c.metricReg != nil {
			c.metricReg.Unregister()
		}
	}()

	// 1. Initialize JetStream / KV for this election
	if err := c.initKV(ctx); err != nil {
		return fmt.Errorf("failed to init election state for %s: %w", c.cfg.KeyName, err)
	}

	// 2. Run Election Loop
	return c.runElectionLoop(ctx)
}

// Shutdown stops the election participation.
// Ideally, the user should cancel the context passed to Run().
func (c *Candidate) Resign(ctx context.Context) error {
	c.mu.Lock()
	isLeader := c.isLeader
	lastRev := c.lastRev
	c.mu.Unlock()

	if isLeader && c.kv != nil {
		return c.kv.Delete(ctx, c.cfg.KeyName, jetstream.LastRevision(lastRev))
	}
	return nil
}

// IsLeader checks if this candidate holds the lease for the configured key.
func (c *Candidate) IsLeader() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.isLeader {
		return false
	}

	// Safety margin check for stale leadership
	safetyMargin := c.cfg.TTL / 10
	if time.Since(c.lastRenewTime) > (c.cfg.TTL - safetyMargin) {
		return false
	}

	return true
}

// CurrentLeader returns the ID of the current leader for this key.
func (c *Candidate) CurrentLeader() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leaderID
}

// LeaderRev returns the current revision of the leader lease.
func (c *Candidate) LeaderRev() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.isLeader {
		return 0
	}
	safetyMargin := c.cfg.TTL / 10
	if time.Since(c.lastRenewTime) > (c.cfg.TTL - safetyMargin) {
		return 0
	}

	return c.lastRev
}

// --- Internal Logic ---

func (c *Candidate) initKV(ctx context.Context) error {
	js, err := jetstream.New(c.nc)
	if err != nil {
		return err
	}
	c.js = js

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout init KV: %v", lastErr)
		case <-ticker.C:
			_, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket:      c.cfg.BucketName,
				Description: "Leader Election State",
				TTL:         c.cfg.TTL,
				Storage:     jetstream.FileStorage,
				Replicas:    c.cfg.Replicas,
			})
			if err == nil {
				kv, err := js.KeyValue(ctx, c.cfg.BucketName)
				if err == nil {
					c.kv = kv
					return nil
				}
			}
			lastErr = err
		}
	}
}

func (c *Candidate) runElectionLoop(ctx context.Context) error {
	go c.watch(ctx)

	renewTicker := time.NewTicker(c.cfg.RenewInterval)
	defer renewTicker.Stop()

	if !c.cfg.Observer {
		c.acquire(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			// Try to resign on exit
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			c.Resign(shutdownCtx)
			cancel()
			return ctx.Err()

		case <-renewTicker.C:
			if c.IsLeader() {
				c.renew(ctx)
				continue
			}
			if !c.cfg.Observer {
				c.acquire(ctx)
			}

		case <-c.campaignCh:
			if !c.cfg.Observer {
				c.acquire(ctx)
			}
		}
	}
}

func (c *Candidate) watch(ctx context.Context) {
	var watcher jetstream.KeyWatcher
	var err error

	for {
		watcher, err = c.kv.Watch(ctx, c.cfg.KeyName)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
			// retry
		}
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-watcher.Updates():
			if !ok {
				return
			}
			if entry == nil {
				continue
			}
			c.handleWatcherEntry(entry)
		}
	}
}

func (c *Candidate) handleWatcherEntry(entry jetstream.KeyValueEntry) {
	c.mu.Lock()

	var (
		shouldInvokeLeaderChange bool
		shouldStepDown           bool
		newLeaderID              string
	)

	baseAttrs := metric.WithAttributes(
		attribute.String("candidate_id", c.cfg.CandidateID),
		attribute.String("bucket", c.cfg.BucketName),
		attribute.String("key", c.cfg.KeyName),
	)

	switch entry.Operation() {
	case jetstream.KeyValuePut:
		newLeaderID = string(entry.Value())
		if c.leaderID != newLeaderID {
			c.leaderID = newLeaderID
			shouldInvokeLeaderChange = true
			// Metric: Record leader change
			c.leaderChanges.Add(context.Background(), 1, baseAttrs)
		}

		if c.isLeader && newLeaderID != c.cfg.CandidateID {
			c.isLeader = false
			c.leaderID = newLeaderID
			c.lastRev = 0
			shouldStepDown = true
		}

	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		c.leaderID = ""
		select {
		case c.campaignCh <- struct{}{}:
		default:
		}
	}

	c.mu.Unlock()

	if shouldStepDown {
		c.invokeSteppingDown()
	}
	if shouldInvokeLeaderChange {
		c.invokeLeaderChange(newLeaderID)
	}
}

func (c *Candidate) acquire(ctx context.Context) {
	if c.IsLeader() {
		return
	}
	rev, err := c.kv.Create(ctx, c.cfg.KeyName, []byte(c.cfg.CandidateID))
	if err == nil {
		c.becomeLeader(rev)
	}
}

func (c *Candidate) renew(ctx context.Context) {
	c.mu.RLock()
	lastRev := c.lastRev
	c.mu.RUnlock()

	rev, err := c.kv.Update(ctx, c.cfg.KeyName, []byte(c.cfg.CandidateID), lastRev)

	if err != nil { // nolint: nestif
		c.invokeRenewFailure(err)

		// Metric: Record renew failure
		baseAttrs := metric.WithAttributes(
			attribute.String("candidate_id", c.cfg.CandidateID),
			attribute.String("bucket", c.cfg.BucketName),
			attribute.String("key", c.cfg.KeyName),
		)
		c.renewErrors.Add(ctx, 1, baseAttrs)

		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyExists) {
			c.stepDown()
		} else {
			c.mu.RLock()
			isStale := time.Since(c.lastRenewTime) > c.cfg.TTL
			c.mu.RUnlock()
			if isStale {
				c.stepDown()
			}
		}
	} else {
		c.mu.Lock()
		c.lastRev = rev
		c.lastRenewTime = time.Now()
		c.mu.Unlock()
	}
}

func (c *Candidate) becomeLeader(rev uint64) {
	c.mu.Lock()
	c.isLeader = true
	c.leaderID = c.cfg.CandidateID
	c.lastRev = rev
	c.lastRenewTime = time.Now()
	c.mu.Unlock()

	c.invokeElected()
}

func (c *Candidate) stepDown() {
	c.mu.Lock()
	wasLeader := c.isLeader
	c.isLeader = false
	c.leaderID = ""
	c.lastRev = 0
	c.lastRenewTime = time.Time{}
	c.mu.Unlock()

	if wasLeader {
		c.invokeSteppingDown()
	}
}

// --- Callback Invocation Helpers ---

func (c *Candidate) invokeElected() {
	if c.cfg.OnElected == nil {
		return
	}
	if c.cfg.SynchronousCallbacks {
		c.cfg.OnElected()
	} else {
		go c.cfg.OnElected()
	}
}

func (c *Candidate) invokeSteppingDown() {
	if c.cfg.OnSteppingDown == nil {
		return
	}
	if c.cfg.SynchronousCallbacks {
		c.cfg.OnSteppingDown()
	} else {
		go c.cfg.OnSteppingDown()
	}
}

func (c *Candidate) invokeLeaderChange(newLeader string) {
	if c.cfg.OnLeaderChange == nil {
		return
	}
	if c.cfg.SynchronousCallbacks {
		c.cfg.OnLeaderChange(newLeader)
	} else {
		go c.cfg.OnLeaderChange(newLeader)
	}
}

func (c *Candidate) invokeRenewFailure(err error) {
	if c.cfg.OnRenewFailure == nil {
		return
	}
	if c.cfg.SynchronousCallbacks {
		c.cfg.OnRenewFailure(err)
	} else {
		go c.cfg.OnRenewFailure(err)
	}
}
