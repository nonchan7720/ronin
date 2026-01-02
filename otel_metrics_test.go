package ronin

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestCandidateInitMetricsObservableCallbackRuns(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
	})

	c, err := newCandidate(nil, CandidateConfig{
		CandidateID:   "cand",
		TTL:           500 * time.Millisecond,
		MeterProvider: mp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c.mu.Lock()
	c.isLeader = true
	c.lastRev = 7
	c.lastRenewTime = time.Now()
	c.mu.Unlock()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %v", err)
	}
}
