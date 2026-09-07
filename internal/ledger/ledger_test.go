package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/require"
)

// attempt builds the record smolllm-go hands the hook. Usage.Total is derived by
// the library from its parts, so it is spelled out here the same way.
func attempt(provider, modelName string, usage smolllm.Usage, err error) smolllm.Attempt {
	record := smolllm.Attempt{
		Provider:   provider,
		Model:      provider + "/" + modelName,
		ModelName:  modelName,
		APIKeyHint: "sk-…cafe",
		Retry:      0,
		Usage:      usage,
		Duration:   time.Second,
		TTFT:       time.Millisecond,
		Err:        nil,
	}
	if err != nil {
		record.Err = &smolllm.LegError{
			Provider:    provider,
			Model:       record.Model,
			ModelName:   modelName,
			APIKeyHint:  record.APIKeyHint,
			Retry:       0,
			StatusCode:  401,
			Disposition: smolllm.DispositionAdvance,
			Err:         err,
		}
	}
	return record
}

func usage(input, output, cacheRead, reasoning int, estimated bool) smolllm.Usage {
	return smolllm.Usage{
		Input:     input,
		Output:    output,
		CacheRead: cacheRead,
		Reasoning: reasoning,
		Total:     input + output + cacheRead,
		Estimated: estimated,
	}
}

func TestLedgerAggregatesAttempts(t *testing.T) {
	t.Parallel()

	ledger := New()
	ledger.Record("fast", attempt("mock", "marvin-7b", usage(11, 7, 4, 3, false), nil))
	ledger.Record("fast", attempt("mock", "marvin-7b", usage(5, 0, 0, 0, true), errors.New("upstream failed")))

	require.Equal(t, []Bucket{{
		Day:               time.Now().UTC().Format(time.DateOnly),
		Alias:             "fast",
		Provider:          "mock",
		Model:             "marvin-7b",
		Requests:          2,
		Failures:          1,
		InputTokens:       16,
		CacheReadTokens:   4,
		OutputTokens:      7,
		ReasoningTokens:   3,
		EstimatedRequests: 1,
	}}, ledger.Snapshot())
}

// The library reports Input with cache reads taken out and Output with reasoning
// left in, so the ledger must not double-count either against the total.
func TestLedgerKeepsCachedAndReasoningTokensSeparate(t *testing.T) {
	t.Parallel()

	ledger := New()
	ledger.Record("fast", attempt("mock", "marvin-7b", usage(10, 20, 90, 15, false), nil))

	bucket := ledger.Snapshot()[0]
	require.Equal(t, 10, bucket.InputTokens)
	require.Equal(t, 90, bucket.CacheReadTokens)
	require.Equal(t, 20, bucket.OutputTokens)
	require.Equal(t, 15, bucket.ReasoningTokens)
}

func TestLedgerCountsEveryRetryOfOneLeg(t *testing.T) {
	t.Parallel()

	ledger := New()
	for retry := range 3 {
		record := attempt("mock", "marvin-7b", usage(4, 0, 0, 0, true), errors.New("overloaded"))
		record.Retry = retry
		ledger.Record("fast", record)
	}

	bucket := ledger.Snapshot()[0]
	require.Equal(t, 3, bucket.Requests)
	require.Equal(t, 3, bucket.Failures)
	require.Equal(t, 12, bucket.InputTokens)
}

func TestLedgerSeparatesAliasAndServedModel(t *testing.T) {
	t.Parallel()

	ledger := New()
	for _, event := range []struct {
		alias string
		model string
	}{
		{alias: "fast", model: "alpha"},
		{alias: "mock/alpha", model: "alpha"},
		{alias: "fast", model: "beta"},
	} {
		ledger.Record(event.alias, attempt("mock", event.model, usage(0, 0, 0, 0, false), nil))
	}

	require.Len(t, ledger.Snapshot(), 3)
}

func TestLedgerPrunesExpiredBuckets(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ledger := New()
	ledger.recordAt(now.Add(-retention-24*time.Hour), "old",
		attempt("mock", "old", usage(0, 0, 0, 0, false), nil))
	ledger.recordAt(now, "current", attempt("mock", "current", usage(0, 0, 0, 0, false), nil))

	buckets := ledger.Snapshot()
	require.Len(t, buckets, 1)
	require.Equal(t, "current", buckets[0].Alias)
}
