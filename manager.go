// Package ronin provides an embedded, self-hosted Leader Election mechanism
// similar to Kubernetes Lease Leader Election.
//
// Instead of relying on an external Kubernetes API Server or etcd,
// it embeds a NATS JetStream server to handle distributed consensus and lease management.
// This allows applications to perform robust leader election in any environment
// (Edge, On-premise, bare-metal) with a single binary.
//
// Architecture:
// This library follows a Manager-Candidate architecture to support multi-tenancy.
//   - Manager: Manages the embedded NATS server and the connection (Singleton per process).
//   - Candidate: Manages a specific leader election logic for a key (Multiple instances per process).
//
// Cluster Requirements:
// Reliable leader election requires a cluster of at least 3 nodes (Managers) to tolerate failures.
//
// Operational Notes:
//
//  1. Shared Bucket Configuration:
//     When multiple Candidates share the same BucketName (e.g., "election_bucket") but use different KeyNames,
//     they MUST use identical bucket configurations (TTL, Replicas, Storage).
//     The Manager enforces this consistency when creating Candidates via Manager.NewCandidate().
//
//  2. Shutdown Order:
//     Ensure Candidates are stopped (by canceling their context) BEFORE shutting down the Manager.
//     If the Manager is stopped first, Candidates will fail to release their leases gracefully (Resign)
//     because the underlying connection will be closed.
package ronin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// ======================================================================================
// Manager: Infrastructure Layer
// Manages the embedded NATS server and the client connection.
// ======================================================================================

type ServeLog struct {
	Debug        bool
	Trace        bool
	TraceVerbose bool
	TraceHeaders bool
}

// ServerConfig defines the configuration for the embedded NATS server.
type ServerConfig struct {
	// NodeID is the unique identifier for this NATS server node (e.g., Pod Name).
	NodeID string

	Host        string // Bind address (default: "0.0.0.0")
	ClientPort  int    // Port for client connections (default: 4222)
	ClusterName string
	ClusterPort int    // Port for server clustering (default: 6222)
	Peers       string // Other nodes to form the consensus cluster (e.g., "nats://node1:6222,nats://node2:6222")

	// DataDir stores the JetStream state (Raft logs).
	// If empty, defaults to "/${TMPDIR:-tmp}/ronin/{NodeID}".
	DataDir string

	// NATSOptions allows full customization of the embedded NATS server.
	NATSOptions func(*server.Options)

	// NATSClientOptions allows customization of the local NATS client connection.
	NATSClientOptions []nats.Option

	Logging *ServeLog
}

// bucketProps holds the configuration properties of a KV bucket.
// Used to ensure consistency across multiple candidates sharing the same bucket.
type bucketProps struct {
	TTL      time.Duration
	Replicas int
}

// Manager manages the embedded NATS server lifecycle and provides a shared connection.
type Manager struct {
	cfg        ServerConfig
	ns         *server.Server
	nc         *nats.Conn
	registryMu sync.Mutex
	registry   map[string]bucketProps
}

// NewManager creates a new Manager instance.
func NewManager(cfg ServerConfig) (*Manager, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("ServerConfig.NodeID is required")
	}
	// Defaults
	if cfg.Host == "" {
		cfg.Host = "0.0.0.0"
	}
	if cfg.ClientPort == 0 {
		cfg.ClientPort = 4222
	}
	if cfg.ClusterPort == 0 {
		cfg.ClusterPort = 6222
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(os.TempDir(), "ronin", cfg.NodeID)
	}
	if cfg.ClusterName == "" {
		cfg.ClusterName = "ronin-cluster"
	}

	return &Manager{
		cfg:      cfg,
		registry: make(map[string]bucketProps),
	}, nil
}

// Start launches the embedded NATS server and establishes a local connection.
// It respects the context deadline for the server startup timeout.
// The caller is responsible for calling Shutdown().
func (m *Manager) Start(ctx context.Context) error {
	// Calculate startup timeout from context
	timeout := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return ctx.Err()
		}
	}

	// 1. Start Embedded NATS Server
	if err := m.startServer(timeout); err != nil {
		return fmt.Errorf("failed to start nats server: %w", err)
	}

	// 2. Connect to Local Server
	if err := m.connect(); err != nil {
		m.shutdownServer() // Cleanup if connect fails
		return fmt.Errorf("failed to connect to local nats: %w", err)
	}

	return nil
}

// Conn returns the shared NATS connection.
// This connection is thread-safe and can be used by multiple Candidates.
func (m *Manager) Conn() *nats.Conn {
	return m.nc
}

// NewCandidate creates a new Candidate attached to this Manager.
// It performs validation to ensure that all Candidates sharing the same BucketName
// have consistent configurations (TTL, Replicas).
func (m *Manager) NewCandidate(cfg CandidateConfig) (*Candidate, error) {
	if m.nc == nil {
		return nil, fmt.Errorf("manager must be started before creating candidates")
	}

	// Registry Check: Ensure bucket config consistency
	m.registryMu.Lock()
	defer m.registryMu.Unlock()

	// Set default Replicas if 0
	replicas := cfg.Replicas
	if replicas == 0 {
		if m.cfg.Peers != "" {
			replicas = 3
		} else {
			replicas = 1
		}
	}
	// Temporarily update cfg to pass the resolved replicas to newCandidate
	cfg.Replicas = replicas

	if props, exists := m.registry[cfg.BucketName]; exists {
		if props.TTL != cfg.TTL {
			return nil, fmt.Errorf("bucket config mismatch for '%s': TTL mismatch (registered=%v, new=%v)", cfg.BucketName, props.TTL, cfg.TTL)
		}
		if props.Replicas != replicas {
			return nil, fmt.Errorf("bucket config mismatch for '%s': Replicas mismatch (registered=%d, new=%d)", cfg.BucketName, props.Replicas, replicas)
		}
	} else {
		m.registry[cfg.BucketName] = bucketProps{
			TTL:      cfg.TTL,
			Replicas: replicas,
		}
	}

	// Use internal unexported constructor to ensure safety
	return newCandidate(m.nc, cfg)
}

// Shutdown closes the connection and stops the embedded NATS server.
func (m *Manager) Shutdown() {
	if m.nc != nil {
		m.nc.Close()
	}
	m.shutdownServer()
}

func (m *Manager) shutdownServer() {
	if m.ns != nil {
		m.ns.Shutdown()
	}
}

func (m *Manager) startServer(timeout time.Duration) error {
	logConfig := m.cfg.Logging
	if logConfig == nil {
		logConfig = &ServeLog{}
	}
	opts := &server.Options{
		ServerName: m.cfg.NodeID,
		Host:       m.cfg.Host,
		Port:       m.cfg.ClientPort,
		Cluster: server.ClusterOpts{
			Name: m.cfg.ClusterName,
			Port: m.cfg.ClusterPort,
		},
		JetStream:       m.cfg.DataDir != "", // Enable JetStream
		StoreDir:        m.cfg.DataDir,
		NoSystemAccount: true, // Default, can be overridden by NATSOptions
		Debug:           logConfig.Debug,
		Logtime:         true,
		Trace:           logConfig.Trace,
		TraceVerbose:    logConfig.TraceVerbose,
		TraceHeaders:    logConfig.TraceHeaders,
	}

	if m.cfg.Peers != "" {
		opts.Routes = server.RoutesFromStr(m.cfg.Peers)
	}

	if m.cfg.NATSOptions != nil {
		m.cfg.NATSOptions(opts)
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		return err
	}
	if m.cfg.Logging != nil {
		ns.ConfigureLogger()
	}
	ns.Start()

	if !ns.ReadyForConnections(timeout) {
		return fmt.Errorf("server start timeout")
	}

	m.ns = ns
	return nil
}

func (m *Manager) connect() error {
	url := fmt.Sprintf("nats://127.0.0.1:%d", m.cfg.ClientPort)

	opts := []nats.Option{
		nats.Name(m.cfg.NodeID + "-manager"),
		nats.MaxReconnects(-1),
	}

	if len(m.cfg.NATSClientOptions) > 0 {
		opts = append(opts, m.cfg.NATSClientOptions...)
	}

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return err
	}
	m.nc = nc
	return nil
}
