package server

import (
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// deadlineRecorder wraps http.DefaultTransport, which smolllm-go's calls ride, and
// remembers the context deadline each upstream request went out with. That is
// what the server actually applied to the call, read where it meets the wire.
type deadlineRecorder struct {
	next         http.RoundTripper
	upstreamHost string

	mu        sync.Mutex
	deadlines []time.Time // zero when the request had no deadline
}

func (d *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == d.upstreamHost {
		deadline, _ := req.Context().Deadline()
		d.mu.Lock()
		d.deadlines = append(d.deadlines, deadline)
		d.mu.Unlock()
	}
	return d.next.RoundTrip(req)
}

// upstreamDeadline runs one chat call and returns the deadline its upstream
// request carried. Not parallel: it swaps the process-wide default transport.
func upstreamDeadline(t *testing.T, body string) time.Time {
	t.Helper()
	ts, _ := newTestRig(t, false)

	upstream, err := url.Parse(os.Getenv("MOCK_BASE_URL"))
	require.NoError(t, err)
	recorder := &deadlineRecorder{next: http.DefaultTransport, upstreamHost: upstream.Host}
	original := http.DefaultTransport
	http.DefaultTransport = recorder
	t.Cleanup(func() { http.DefaultTransport = original })

	resp := postChat(t, ts, body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.Len(t, recorder.deadlines, 1)
	return recorder.deadlines[0]
}

// A call longer than smolllm-go's 600s default died with 504 although its
// client held an 1800s budget. With no `timeout` field the server bounds
// nothing: the client's own deadline and hang-up already do.
func TestChatCompletions_NoTimeoutFieldLeavesTheCallUnbounded(t *testing.T) {
	deadline := upstreamDeadline(t, `{"model":"mock/marvin-7b","messages":[{"role":"user","content":"hi"}]}`)
	require.True(t, deadline.IsZero(), "upstream request carried deadline %v", deadline)
}

// A request that does name a budget still gets exactly it, overriding the
// unbounded default.
func TestChatCompletions_TimeoutFieldBoundsTheCall(t *testing.T) {
	before := time.Now()
	deadline := upstreamDeadline(t,
		`{"model":"mock/marvin-7b","timeout":1800,"messages":[{"role":"user","content":"hi"}]}`)
	require.False(t, deadline.IsZero(), "the timeout field was not applied")
	require.WithinDuration(t, before.Add(1800*time.Second), deadline, 30*time.Second)
}
