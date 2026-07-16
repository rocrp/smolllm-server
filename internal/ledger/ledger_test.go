package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/require"
)

func TestLedgerAggregatesAttempts(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ledger := New()
	ledger.Record("fast", smolllm.RequestEvent{
		Usage: smolllm.Usage{
			Provider:     "mock",
			ModelName:    "marvin-7b",
			InputTokens:  11,
			OutputTokens: 7,
		},
		Timestamp: now,
	})
	ledger.Record("fast", smolllm.RequestEvent{
		Usage: smolllm.Usage{
			Provider:    "mock",
			ModelName:   "marvin-7b",
			InputTokens: 5,
			Estimated:   true,
		},
		Error:     errors.New("upstream failed"),
		Timestamp: now,
	})

	require.Equal(t, []Bucket{{
		Day:               now.Format(time.DateOnly),
		Alias:             "fast",
		Provider:          "mock",
		Model:             "marvin-7b",
		Requests:          2,
		Failures:          1,
		InputTokens:       16,
		OutputTokens:      7,
		EstimatedRequests: 1,
	}}, ledger.Snapshot())
}

func TestLedgerSeparatesAliasAndServedModel(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ledger := New()
	for _, event := range []struct {
		alias    string
		provider string
		model    string
	}{
		{alias: "fast", provider: "mock", model: "alpha"},
		{alias: "mock/alpha", provider: "mock", model: "alpha"},
		{alias: "fast", provider: "mock", model: "beta"},
	} {
		ledger.Record(event.alias, smolllm.RequestEvent{
			Usage:     smolllm.Usage{Provider: event.provider, ModelName: event.model},
			Timestamp: now,
		})
	}

	require.Len(t, ledger.Snapshot(), 3)
}

func TestLedgerPrunesExpiredBuckets(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ledger := New()
	ledger.Record("old", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "mock", ModelName: "old"},
		Timestamp: now.Add(-retention - 24*time.Hour),
	})
	ledger.Record("current", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "mock", ModelName: "current"},
		Timestamp: now,
	})

	buckets := ledger.Snapshot()
	require.Len(t, buckets, 1)
	require.Equal(t, "current", buckets[0].Alias)
}
