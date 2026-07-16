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
// provider/model.
type Bucket struct {
	Day               string `json:"day"`
	Alias             string `json:"alias"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	Requests          int    `json:"requests"`
	Failures          int    `json:"failures"`
	InputTokens       int    `json:"input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
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

// Hook returns a request hook bound to the alias or raw model requested by the
// client.
func (l *Ledger) Hook(alias string) func(smolllm.RequestEvent) {
	return func(event smolllm.RequestEvent) {
		l.Record(alias, event)
	}
}

// Record adds one library request event to its stats bucket.
func (l *Ledger) Record(alias string, event smolllm.RequestEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := bucketKey{
		day:      event.Timestamp.UTC().Format(time.DateOnly),
		alias:    alias,
		provider: event.Provider,
		model:    event.ModelName,
	}
	bucket := l.buckets[key]
	bucket.Day = key.day
	bucket.Alias = key.alias
	bucket.Provider = key.provider
	bucket.Model = key.model
	bucket.Requests++
	if event.Error != nil {
		bucket.Failures++
	}
	bucket.InputTokens += event.InputTokens
	bucket.OutputTokens += event.OutputTokens
	if event.Estimated {
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
