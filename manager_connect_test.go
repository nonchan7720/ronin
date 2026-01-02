package ronin

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestManagerStartWithClientOptionsAndNoLogging(t *testing.T) {
	t.Parallel()
	ports := getFreePorts(t, 1)
	m, err := NewManager(ServerConfig{
		NodeID:      "node-client-opts",
		Host:        "127.0.0.1",
		ClientPort:  ports[0],
		DataDir:     t.TempDir(),
		Logging:     nil,
		NATSOptions: func(o *server.Options) { o.Cluster = server.ClusterOpts{}; o.NoLog = true; o.NoSigs = true },
		NATSClientOptions: []nats.Option{
			nats.NoReconnect(),
		},
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

	if m.Conn() == nil {
		t.Fatalf("expected non-nil connection")
	}
}
