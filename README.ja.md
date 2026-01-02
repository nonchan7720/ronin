# Ronin

Masterless High-Availability Leader Election for Go.

[English](README.md)

Ronin は、Kubernetes Lease Leader Election に近いセマンティクスのリーダー選出を、外部の etcd や Kubernetes API Server に依存せずに提供する Go ライブラリです。NATS JetStream (Raft) を合意形成エンジンとして内包し、単一バイナリでクラスタ運用と選出ロジックを組み込めます。

## 特徴

- 外部ミドルウェア不要: embedded NATS + JetStream
- Safety 重視: 二重リーダー防止 / stale leader の自律退避
- Manager/Candidate モデル: 1プロセスで複数キー（シャード）の選出を効率運用
- OpenTelemetry: leader 状態や失敗回数をメトリクス出力

## 使い方

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

	// 1) Manager 起動（embedded NATS server + shared connection）
	mgr, err := ronin.NewManager(ronin.ServerConfig{
		NodeID: "node-1", // required
		// Host, ClientPort, ClusterPort, DataDir は省略するとデフォルトが入ります。
		// Peers: "nats://node-2:6222,nats://node-3:6222", // 複数ノードでクラスタ構成
	})
	if err != nil {
		log.Fatalf("new manager: %v", err)
	}
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("start manager: %v", err)
	}
	defer mgr.Shutdown()

	// 2) 特定の bucket/key に対する Candidate 作成
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

	// Application loop
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

アプリケーション側で OpenTelemetry SDK を設定していれば、Ronin はメトリクスを出力します。必要に応じて CandidateConfig の MeterProvider を差し替えできます。

| Metric name                   | Type    | Description                                           |
| ----------------------------- | ------- | ----------------------------------------------------- |
| election.leader_status        | Gauge   | leader なら 1、そうでなければ 0                       |
| election.leader_term          | Gauge   | NATS KV の revision（フェンシングトークンとして有用） |
| election.renew_failures_total | Counter | renew 失敗回数                                        |
| election.leader_changes_total | Counter | 観測した leader 交代回数                              |

全てのメトリクスに `candidate_id`, `bucket`, `key` 属性が付きます。

## Operational notes

### クラスタ要件

信頼性の高い選出には quorum が必要です。

- 3ノード以上（推奨）: 1台障害に耐性
- 1ノード: 開発・単体用途向け（SPOF）

### Bucket 設定の共有

同じ BucketName を複数 Candidate で共有する場合、TTL と Replicas は一致している必要があります（Manager が検証します）。

- CandidateConfig.Replicas が 0 の場合、単体（Peers なし）では 1、クラスタ（Peers あり）では 3 がデフォルトになります。

### シャットダウン順序

Candidate を停止（Run に渡した ctx を cancel）してから Manager を停止してください。先に Manager を止めると Candidate がリース解放（Resign）できず、次の選出が TTL まで遅れることがあります。

## License

MIT License
