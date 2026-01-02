package ronin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
)

type failMeterProvider struct {
	metric.MeterProvider
	meter metric.Meter
}

func (p failMeterProvider) Meter(_ string, _ ...metric.MeterOption) metric.Meter {
	return p.meter
}

type failMeter struct {
	metric.Meter
	failOn string
}

func (m failMeter) Int64ObservableGauge(name string, options ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	if name == m.failOn {
		return nil, errors.New("forced error")
	}
	return m.Meter.Int64ObservableGauge(name, options...)
}

func (m failMeter) Int64Counter(name string, options ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	if name == m.failOn {
		return nil, errors.New("forced error")
	}
	return m.Meter.Int64Counter(name, options...)
}

func (m failMeter) RegisterCallback(f metric.Callback, instruments ...metric.Observable) (metric.Registration, error) {
	if m.failOn == "RegisterCallback" {
		return nil, errors.New("forced error")
	}
	return m.Meter.RegisterCallback(f, instruments...)
}

func TestCandidateInitMetricsErrorPaths(t *testing.T) {
	t.Parallel()
	// Use a real no-op meter as the base, then wrap it to force errors.
	baseProvider := metricnoop.NewMeterProvider()
	base := baseProvider.Meter("base")

	tests := []struct {
		name   string
		failOn string
	}{
		{"fail_leader_status_gauge", "election.leader_status"},
		{"fail_leader_term_gauge", "election.leader_term"},
		{"fail_renew_counter", "election.renew_failures_total"},
		{"fail_leader_changes_counter", "election.leader_changes_total"},
		{"fail_register_callback", "RegisterCallback"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mp := failMeterProvider{
				MeterProvider: baseProvider,
				meter:         failMeter{Meter: base, failOn: tc.failOn},
			}

			_, err := newCandidate(nil, CandidateConfig{
				CandidateID:   "cand",
				MeterProvider: mp,
			})
			if err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestCandidateInitKVCanceledContext(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-initkv-cancel", ports[0], -1, "")

	cand, err := mgr.NewCandidate(CandidateConfig{CandidateID: "cand", BucketName: "b", KeyName: "k"})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cand.initKV(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestCandidateWatchRetriesAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-watch-cancel", ports[0], -1, "")

	cand, err := mgr.NewCandidate(CandidateConfig{CandidateID: "cand", BucketName: "b", KeyName: "k"})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cand.initKV(ctx); err != nil {
		t.Fatalf("initKV failed: %v", err)
	}

	// Force Watch() to fail by closing the underlying connection, then cancel quickly.
	mgr.Conn().Close()
	watchCtx, watchCancel := context.WithCancel(context.Background())
	watchCancel()
	cand.watch(watchCtx)
}

func TestCandidateRunInitKVErrorPath(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	mgr := setupClusterNode(t, "node-run-initkv", ports[0], -1, "")

	cand, err := mgr.NewCandidate(CandidateConfig{CandidateID: "cand", BucketName: "b", KeyName: "k"})
	if err != nil {
		t.Fatalf("failed to create candidate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cand.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled, got %v", err)
	}
}

func TestManagerStartWithoutDeadline(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	m, err := NewManager(ServerConfig{
		NodeID:     "node-no-deadline",
		Host:       "127.0.0.1",
		ClientPort: ports[0],
		DataDir:    t.TempDir(),
		NATSOptions: func(o *server.Options) {
			o.Cluster = server.ClusterOpts{}
			o.NoLog = true
			o.NoSigs = true
		},
		NATSClientOptions: []nats.Option{nats.NoReconnect()},
		Logging:           nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m.Shutdown()
}

func TestManagerNewCandidateDefaultReplicasWhenPeersSet(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 2)
	peers := "nats://127.0.0.1:6222" // value just needs to be non-empty to exercise the branch

	m, err := NewManager(ServerConfig{
		NodeID:      "node-peers-default",
		Host:        "127.0.0.1",
		ClientPort:  ports[0],
		ClusterPort: ports[1],
		Peers:       peers,
		DataDir:     t.TempDir(),
		NATSOptions: func(o *server.Options) {
			// Run standalone while still keeping cfg.Peers non-empty for logic coverage.
			o.Cluster = server.ClusterOpts{}
			o.Routes = nil
			o.NoLog = true
			o.NoSigs = true
		},
		Logging: nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	defer m.Shutdown()

	c, err := m.NewCandidate(CandidateConfig{
		CandidateID: "cand",
		BucketName:  "b-peers",
		KeyName:     "k-peers",
		TTL:         1 * time.Second,
		Replicas:    0,
	})
	if err != nil {
		t.Fatalf("new candidate failed: %v", err)
	}
	if c.cfg.Replicas != 3 {
		t.Fatalf("expected replicas default to 3 when peers set, got %d", c.cfg.Replicas)
	}
}
