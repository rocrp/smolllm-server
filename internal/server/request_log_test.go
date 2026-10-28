package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/stretchr/testify/require"
)

// syncBuffer lets the server's goroutines write the log while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) requestLine(t *testing.T) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		if strings.Contains(line, "msg=request ") {
			return line
		}
	}
	t.Fatalf("no request log line in:\n%s", b.buf.String())
	return ""
}

// newCacheRig serves alias `fast` = smolayer/alpha,mock/beta from one upstream.
// alpha answers as `alpha`, anything else as a clean success; every request
// body is recorded.
func newCacheRig(t *testing.T, alpha http.HandlerFunc) (*httptest.Server, *syncBuffer, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if body["model"] == "alpha" {
			alpha(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"beta answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	for _, provider := range []string{"SMOLAYER", "MOCK"} {
		t.Setenv(provider+"_BASE_URL", upstream.URL)
		t.Setenv(provider+"_API_KEY", "test-key")
	}

	logs := &syncBuffer{}
	cfg := &config.Config{
		Server:  config.ServerConfig{AccessKey: "rocry"},
		Aliases: map[string]string{"fast": "smolayer/alpha,mock/beta"},
	}
	ts := httptest.NewServer(New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(logs, nil))).HTTP.Handler)
	t.Cleanup(ts.Close)
	return ts, logs, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

func rateLimited(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
}

// prompt_cache_key and user reach only providers that accept them: a fallback
// provider may reject an unknown field.
func TestChatCompletions_PromptCacheKeyReachesOnlyCacheAwareProviders(t *testing.T) {
	ts, _, bodies := newCacheRig(t, rateLimited)

	resp := postChat(t, ts, `{"model":"fast","prompt_cache_key":"conv-7","user":"mnemo",`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	sent := bodies()
	require.Len(t, sent, 2)
	require.Equal(t, "conv-7", sent[0]["prompt_cache_key"])
	require.Equal(t, "mnemo", sent[0]["user"])
	require.NotContains(t, sent[1], "prompt_cache_key")
	require.NotContains(t, sent[1], "user")
}

func TestRequestLog_NamesAliasAndServedModel(t *testing.T) {
	ts, logs, _ := newCacheRig(t, rateLimited)

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	line := logs.requestLine(t)
	require.Contains(t, line, "status=200")
	require.Contains(t, line, "key=default", "the single access_key is logged by its name")
	require.NotContains(t, line, "rocry", "never the key itself")
	require.Contains(t, line, "alias=fast")
	require.Contains(t, line, "model=mock/beta")
}

// A caller's X-Request-ID lands on the request line, so its logs and ours join on it.
func TestRequestLog_CarriesTheForwardedRequestID(t *testing.T) {
	ts, logs, _ := newCacheRig(t, rateLimited)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "msg-1-7-3")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	require.Contains(t, logs.requestLine(t), "request_id=msg-1-7-3")
}

// A stream that fails after its 200 went out is logged with the failure's
// status, not the 200 the wire carried.
func TestRequestLog_FailedStreamIsNotLoggedAs200(t *testing.T) {
	ts, logs, _ := newCacheRig(t, rateLimited)
	t.Setenv("MOCK_BASE_URL", "http://127.0.0.1:1") // beta is unreachable too

	resp := postChat(t, ts, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the status line went out before the chain failed")
	require.Contains(t, string(data), `"finish_reason":"error"`)

	line := logs.requestLine(t)
	require.NotContains(t, line, "status=200")
	require.Contains(t, line, "status=502")
	require.Contains(t, line, "level=WARN")
	require.Contains(t, line, "alias=fast")
	require.Contains(t, line, "model=mock/beta")
}

// A smolayer leg that fails mid-stream reports it in band; the chain counts
// that as a failed leg and fails over, and the answer carries only the fallback.
func TestChatCompletions_InBandStreamErrorFailsOver(t *testing.T) {
	ts, _, bodies := newCacheRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}],"+
			"\"error\":{\"message\":\"the upstream closed the stream before the turn finished\"}}\n\ndata: [DONE]\n\n")
	})

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))
	require.Contains(t, string(data), "beta answer")
	require.NotContains(t, string(data), "partial")
	require.Len(t, bodies(), 2)
}
