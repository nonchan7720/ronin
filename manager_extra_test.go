package ronin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

func TestNewManagerValidationAndDefaults(t *testing.T) {
	t.Parallel()
	_, err := NewManager(ServerConfig{})
	if err == nil {
		t.Fatalf("expected error for missing NodeID")
	}

	m, err := NewManager(ServerConfig{NodeID: "n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.cfg.Host == "" || m.cfg.ClientPort == 0 || m.cfg.ClusterPort == 0 || m.cfg.ClusterName == "" || m.cfg.DataDir == "" {
		t.Fatalf("expected defaults to be applied")
	}
}

func TestManagerConnAndNewCandidateBeforeStart(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	m, err := NewManager(ServerConfig{NodeID: "node", Host: "127.0.0.1", ClientPort: ports[0]})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if m.Conn() != nil {
		t.Fatalf("expected nil conn before Start")
	}

	_, err = m.NewCandidate(CandidateConfig{CandidateID: "c"})
	if err == nil {
		t.Fatalf("expected error creating candidate before start")
	}
}

func TestManagerStartDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	m, err := NewManager(ServerConfig{NodeID: "node-deadline", Host: "127.0.0.1", ClientPort: ports[0]})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pastCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	err = m.Start(pastCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestManagerStartConnectFailureCleansUp(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	m, err := NewManager(ServerConfig{
		NodeID:     "node-connect-fail",
		Host:       "127.0.0.2",
		ClientPort: ports[0],
		DataDir:    t.TempDir(),
		NATSOptions: func(o *server.Options) {
			o.NoLog = true
			o.NoSigs = true
			o.Cluster = server.ClusterOpts{}
		},
		Logging: &ServeLog{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Server listens on 127.0.0.2, but connect() always uses 127.0.0.1, so this must fail.
	err = m.Start(ctx)
	if err == nil {
		t.Fatalf("expected connect failure")
	}

	// Should still be safe to call Shutdown.
	m.Shutdown()
}
