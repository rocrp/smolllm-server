// Package ledger aggregates per-attempt LLM usage in memory.
package ledger

import (
	"sort"
	"sync"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
)

const retention = 31 * 24 * time.Hour

// Bucket aggregates attempts sharing a UTC day, requested alias, and served
// provider/model. The token columns follow smolllm-go's Usage: InputTokens
// EXCLUDES cache reads, which are counted on their own, and OutputTokens
// INCLUDES the reasoning tokens reported beside it.
type Bucket struct {
	Day               string `json:"day"`
	Alias             string `json:"alias"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	Requests          int    `json:"requests"`
	Failures          int    `json:"failures"`
	InputTokens       int    `json:"input_tokens"`
	CacheReadTokens   int    `json:"cache_read_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	ReasoningTokens   int    `json:"reasoning_tokens"`
	EstimatedRequests int    `json:"estimated_requests"`
}

type bucketKey struct {
	day      string
	alias    string
	provider string
	model    string
}

// Ledger is a concurrency-safe, process-local usage ledger.
type Ledger struct {
	mu      sync.Mutex
	buckets map[bucketKey]Bucket
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{buckets: make(map[bucketKey]Bucket)}
}

// Hook returns a smolllm-go request hook bound to the alias or raw model string
// the client asked for. It fires once per attempt, successful or not.
func (l *Ledger) Hook(alias string) func(smolllm.Attempt) {
	return func(attempt smolllm.Attempt) {
		l.Record(alias, attempt)
	}
}

// Record adds one attempt to its stats bucket, dated now. smolllm-go stopped
// timestamping attempts in v0.3, and the hook fires as each leg finishes, so the
// moment of recording is the moment the attempt ended.
func (l *Ledger) Record(alias string, attempt smolllm.Attempt) {
	l.recordAt(time.Now().UTC(), alias, attempt)
}

func (l *Ledger) recordAt(at time.Time, alias string, attempt smolllm.Attempt) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := bucketKey{
		day:      at.UTC().Format(time.DateOnly),
		alias:    alias,
		provider: attempt.Provider,
		model:    attempt.ModelName,
	}
	bucket := l.buckets[key]
	bucket.Day = key.day
	bucket.Alias = key.alias
	bucket.Provider = key.provider
	bucket.Model = key.model
	bucket.Requests++
	if attempt.Failed() {
		bucket.Failures++
	}
	bucket.InputTokens += attempt.Usage.Input
	bucket.CacheReadTokens += attempt.Usage.CacheRead
	bucket.OutputTokens += attempt.Usage.Output
	bucket.ReasoningTokens += attempt.Usage.Reasoning
	if attempt.Usage.Estimated {
		bucket.EstimatedRequests++
	}
	l.buckets[key] = bucket
	l.pruneLocked(time.Now().UTC())
}

// Snapshot returns a stable, sorted copy of retained buckets.
func (l *Ledger) Snapshot() []Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(time.Now().UTC())
	buckets := make([]Bucket, 0, len(l.buckets))
	for _, bucket := range l.buckets {
		buckets = append(buckets, bucket)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Day != buckets[j].Day {
			return buckets[i].Day < buckets[j].Day
		}
		if buckets[i].Alias != buckets[j].Alias {
			return buckets[i].Alias < buckets[j].Alias
		}
		if buckets[i].Provider != buckets[j].Provider {
			return buckets[i].Provider < buckets[j].Provider
		}
		return buckets[i].Model < buckets[j].Model
	})
	return buckets
}

func (l *Ledger) pruneLocked(now time.Time) {
	cutoffDay := now.Add(-retention).Format(time.DateOnly)
	for key := range l.buckets {
		if key.day < cutoffDay {
			delete(l.buckets, key)
		}
	}
}
