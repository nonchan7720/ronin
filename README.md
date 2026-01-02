# Ronin

Masterless High-Availability Leader Election for Go.

[日本語](./README.ja.md)

Ronin is a Go library that provides leader election semantics similar to Kubernetes Lease Leader Election without depending on an external etcd or Kubernetes API Server. It embeds NATS JetStream (Raft) as the consensus engine so you can ship leader election as part of a single binary.

## Features

- No external middleware: embedded NATS + JetStream
- Safety-first: prevents double leaders and self-steps down on stale leadership
- Manager/Candidate model: efficiently manage many election keys (shards) per process
- OpenTelemetry: exports metrics for leadership status and failures

## Usage

### Install

```bash
go get github.com/nonchan7720/ronin
```

### Minimal example

<!-- markdownlint-disable MD010 -->
```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/nonchan7720/ronin"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1) Start manager (embedded NATS server + shared connection)
	mgr, err := ronin.NewManager(ronin.ServerConfig{
		NodeID: "node-1", // required
		// Host, ClientPort, ClusterPort, DataDir have defaults.
		// Peers: "nats://node-2:6222,nats://node-3:6222", // multi-node cluster
	})
	if err != nil {
		log.Fatalf("new manager: %v", err)
	}
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("start manager: %v", err)
	}
	defer mgr.Shutdown()

	// 2) Create candidate for a specific bucket/key
	cand, err := mgr.NewCandidate(ronin.CandidateConfig{
		CandidateID: "worker-1", // required
		BucketName:  "election_bucket",
		KeyName:     "shard_01",
		TTL:         5 * time.Second,
		OnElected: func() {
			log.Println("became leader")
		},
		OnSteppingDown: func() {
			log.Println("lost leadership")
		},
		OnLeaderChange: func(id string) {
			log.Printf("leader changed: %s", id)
		},
	})
	if err != nil {
		log.Fatalf("new candidate: %v", err)
	}

	go func() {
		if err := cand.Run(ctx); err != nil {
			log.Printf("candidate stopped: %v", err)
		}
	}()

	for {
		select {
		case <-time.After(1 * time.Second):
			if cand.IsLeader() {
				// Use cand.LeaderRev() as a fencing token if needed.
				log.Println("do leader work")
			}
		case <-ctx.Done():
			return
		}
	}
}
```
<!-- markdownlint-enable MD010 -->

## Observability (OpenTelemetry)

If your application configures the OpenTelemetry SDK, Ronin exports metrics automatically. You can also provide a custom MeterProvider via CandidateConfig.

| Metric name                   | Type    | Description                                  |
| ----------------------------- | ------- | -------------------------------------------- |
| election.leader_status        | Gauge   | 1 if leader, otherwise 0                     |
| election.leader_term          | Gauge   | NATS KV revision (useful as a fencing token) |
| election.renew_failures_total | Counter | Number of renew failures                     |
| election.leader_changes_total | Counter | Number of observed leader changes            |

All metrics include `candidate_id`, `bucket`, and `key` attributes.

## Operational notes

### Cluster requirements

Reliable leader election needs a quorum.

- 3+ nodes (recommended): tolerates one node failure
- 1 node: suitable for development/single-node use (SPOF)

### Shared bucket configuration

If multiple candidates share the same BucketName, their TTL and Replicas must match (enforced by Manager).

- If CandidateConfig.Replicas is 0, the default is 1 for single-node (no Peers) and 3 for clustered mode (Peers set).

### Shutdown order

Stop Candidates first (cancel the ctx passed to Run), then shut down the Manager. If you stop the Manager first, Candidates may fail to resign gracefully and the next election can be delayed until TTL expires.

## License

MIT License
