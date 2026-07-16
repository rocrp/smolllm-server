package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/require"
)

func TestStats_AggregatesChatAndEmbeddingAttempts(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)

	requests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"mock/marvin-7b","messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/embeddings", body: `{"model":"fast","input":"hi"}`},
	}
	for _, request := range requests {
		req, err := http.NewRequest(http.MethodPost, ts.URL+request.path, strings.NewReader(request.body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer rocry")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
	}

	unauthorized, err := http.Get(ts.URL + "/stats")
	require.NoError(t, err)
	defer unauthorized.Body.Close()
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/stats", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Buckets []ledger.Bucket `json:"buckets"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Buckets, 2)
	day := out.Buckets[0].Day
	require.Equal(t, []ledger.Bucket{
		{
			Day:          day,
			Alias:        "fast",
			Provider:     "mock",
			Model:        "marvin-7b",
			Requests:     3,
			InputTokens:  8,
			OutputTokens: 2,
		},
		{
			Day:          day,
			Alias:        "mock/marvin-7b",
			Provider:     "mock",
			Model:        "marvin-7b",
			Requests:     1,
			InputTokens:  3,
			OutputTokens: 1,
		},
	}, out.Buckets)
}

func TestStats_RecordsFailedAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "warn"},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(config.NewStore("", cfg), logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)

	requests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/embeddings", body: `{"model":"fast","input":"hi"}`},
	}
	for _, request := range requests {
		failedReq, err := http.NewRequest(http.MethodPost, ts.URL+request.path, strings.NewReader(request.body))
		require.NoError(t, err)
		failedReq.Header.Set("Authorization", "Bearer rocry")
		failedReq.Header.Set("Content-Type", "application/json")
		failedResp, err := http.DefaultClient.Do(failedReq)
		require.NoError(t, err)
		defer failedResp.Body.Close()
		require.Equal(t, http.StatusBadGateway, failedResp.StatusCode)
	}

	statsReq, err := http.NewRequest(http.MethodGet, ts.URL+"/stats", nil)
	require.NoError(t, err)
	statsReq.Header.Set("Authorization", "Bearer rocry")
	statsResp, err := http.DefaultClient.Do(statsReq)
	require.NoError(t, err)
	defer statsResp.Body.Close()
	require.Equal(t, http.StatusOK, statsResp.StatusCode)

	var out struct {
		Buckets []ledger.Bucket `json:"buckets"`
	}
	require.NoError(t, json.NewDecoder(statsResp.Body).Decode(&out))
	require.Len(t, out.Buckets, 1)
	require.Equal(t, "fast", out.Buckets[0].Alias)
	require.Equal(t, "mock", out.Buckets[0].Provider)
	require.Equal(t, "marvin-7b", out.Buckets[0].Model)
	require.Equal(t, 2, out.Buckets[0].Requests)
	require.Equal(t, 2, out.Buckets[0].Failures)
	require.Equal(t, 2, out.Buckets[0].EstimatedRequests)
}
