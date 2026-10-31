package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/stretchr/testify/require"
)

// imageCall is what the stand-in smolayer saw on one request.
type imageCall struct {
	path, auth, requestID, contentType string
	body                               []byte
}

type imageRig struct {
	ts    *httptest.Server
	logs  *syncBuffer
	mu    sync.Mutex
	calls []imageCall
}

func (g *imageRig) seen() []imageCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]imageCall(nil), g.calls...)
}

// newImageRig serves image alias `cover` = smolayer/codex/gpt-image-2 and chat
// alias `fast` from one stand-in smolayer, which answers with reply. Not
// parallel: the smolayer base URL and key come from the process environment.
func newImageRig(t *testing.T, imageTimeout time.Duration, reply http.HandlerFunc) *imageRig {
	t.Helper()
	g := &imageRig{logs: &syncBuffer{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		g.mu.Lock()
		g.calls = append(g.calls, imageCall{
			path: r.URL.Path, auth: r.Header.Get("Authorization"), requestID: r.Header.Get("X-Request-ID"),
			contentType: r.Header.Get("Content-Type"), body: body,
		})
		g.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("SMOLAYER_BASE_URL", upstream.URL)
	t.Setenv("SMOLAYER_API_KEY", "smolayer-secret")

	cfg := &config.Config{
		Server:       config.ServerConfig{AccessKey: "rocry", ImageTimeout: imageTimeout},
		Aliases:      map[string]string{"fast": "smolayer/alpha"},
		ImageAliases: map[string]string{"cover": "smolayer/codex/gpt-image-2"},
	}
	g.ts = httptest.NewServer(New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(g.logs, nil))).HTTP.Handler)
	t.Cleanup(g.ts.Close)
	return g
}

func (g *imageRig) post(t *testing.T, path, body string, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.ts.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

// The request zelna's Core sends (RoCry/zelna#801), and a reply laid out the
// way smolayer writes it: odd spacing and key order that a decode-and-encode
// round trip would change.
const (
	coverRequest = `{"model":"cover","prompt":"A quiet harbour, 晨光","n":1,"size":"auto","quality":"auto",` +
		`"background":"auto","response_format":"b64_json"}`
	coverReply = `{"created":1761900000, "data":[{"b64_json":"iVBORw0KGgo="}],"size":"1383x1137",` +
		`"quality":"low","background":"opaque","generation_id":"gen_7"}` + "\n"
)

func TestImages_ForwardsOneCallAndReturnsTheReplyUntouched(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, coverReply)
	})

	resp := g.post(t, "/v1/images/generations", coverRequest, "X-Request-ID", "cover-1-2026-10-31")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	require.Equal(t, coverReply, string(readAll(t, resp)))

	calls := g.seen()
	require.Len(t, calls, 1)
	require.Equal(t, "/v1/images/generations", calls[0].path)
	require.Equal(t, "Bearer smolayer-secret", calls[0].auth)
	require.Equal(t, "application/json", calls[0].contentType)
	require.Equal(t, "cover-1-2026-10-31", calls[0].requestID)

	// Only `model` changes; every other field reaches smolayer as sent.
	var sent map[string]any
	require.NoError(t, json.Unmarshal(calls[0].body, &sent))
	require.Equal(t, map[string]any{
		"model": "codex/gpt-image-2", "prompt": "A quiet harbour, 晨光", "n": float64(1), "size": "auto",
		"quality": "auto", "background": "auto", "response_format": "b64_json",
	}, sent)
}

func TestImages_PassesUpstreamFailuresThroughAfterOneCall(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"image_timeout", http.StatusGatewayTimeout,
			`{"error":{"message":"image generation timed out after 300s","type":"api_error","code":"image_timeout"}}`},
		{"capacity", http.StatusTooManyRequests,
			`{"error":{"message":"no image slot free","type":"api_error","code":"image_capacity_exhausted"}}`},
		{"bad field", http.StatusBadRequest,
			`{"error":{"message":"unknown size","type":"invalid_request_error","param":"size","code":null}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newImageRig(t, config.DefaultImageTimeout, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			resp := g.post(t, "/v1/images/generations", coverRequest)
			require.Equal(t, tt.status, resp.StatusCode)
			require.Equal(t, tt.body, string(readAll(t, resp)))
			require.Len(t, g.seen(), 1, "an image call is never retried or failed over")
		})
	}
}

func TestImages_RefusesTheWrongKindOfAliasBeforeAnyUpstreamCall(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(http.ResponseWriter, *http.Request) {})
	for _, tt := range []struct{ name, path, body, want string }{
		{"chat alias on images", "/v1/images/generations", `{"model":"fast","prompt":"x"}`, "fast"},
		{"direct model on images", "/v1/images/generations", `{"model":"smolayer/codex/gpt-image-2","prompt":"x"}`, "smolayer/codex"},
		{"image alias on chat", "/v1/chat/completions",
			`{"model":"cover","messages":[{"role":"user","content":"hi"}]}`, "cover"},
		{"no model", "/v1/images/generations", `{"prompt":"x"}`, "model is required"},
		{"not JSON", "/v1/images/generations", `prompt=x`, "invalid request body"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := g.post(t, tt.path, tt.body)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.Contains(t, string(readAll(t, resp)), tt.want)
		})
	}
	require.Empty(t, g.seen())
}

func TestImages_RequiresAnAccessKey(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(http.ResponseWriter, *http.Request) {})
	resp, err := http.Post(g.ts.URL+"/v1/images/generations", "application/json", strings.NewReader(coverRequest))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, readStatus(resp))
	require.Empty(t, g.seen())
}

func readStatus(resp *http.Response) int {
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The server's own deadline, not the client's, ends a call smolayer never
// answers; the client sees a 504 and smolayer saw the request once.
func TestImages_DeadlineEndsAStuckCallWith504(t *testing.T) {
	g := newImageRig(t, 100*time.Millisecond, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	resp := g.post(t, "/v1/images/generations", coverRequest)
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	require.Contains(t, string(readAll(t, resp)), "image_timeout")
	require.Len(t, g.seen(), 1)
}

func TestImages_RequestLogNamesKeyAliasModelAndRequestID(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, coverReply) })
	readAll(t, g.post(t, "/v1/images/generations", coverRequest, "X-Request-ID", "cover-9"))

	line := g.logs.requestLine(t)
	for _, want := range []string{
		"path=/v1/images/generations", "status=200", "key=default", "alias=cover",
		"model=smolayer/codex/gpt-image-2", "request_id=cover-9", "duration_ms=",
	} {
		require.Contains(t, line, want)
	}
	require.NotContains(t, g.logs.String(), "smolayer-secret")
}

// smolayer bounds its own reply at 64 MiB; the server refuses to relay more
// rather than hand the client a truncated image.
func TestImages_ReplyOverSmolayersBoundIsAn502(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("a", 1<<20))
		for range 65 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	resp := g.post(t, "/v1/images/generations", coverRequest)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	require.Contains(t, string(readAll(t, resp)), "64 MiB")
}

func TestImages_RequestBodyIsCapped(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(http.ResponseWriter, *http.Request) {})
	huge := `{"model":"cover","prompt":"` + strings.Repeat("a", 1<<20) + `"}`
	resp := g.post(t, "/v1/images/generations", huge)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	require.Empty(t, g.seen())
}

func TestModels_ListsImageAliasesWithTheirOperation(t *testing.T) {
	g := newImageRig(t, config.DefaultImageTimeout, func(http.ResponseWriter, *http.Request) {})
	req, err := http.NewRequest(http.MethodGet, g.ts.URL+"/v1/models", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	var out struct {
		Data []struct {
			ID                  string   `json:"id"`
			SupportedOperations []string `json:"supported_operations"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(readAll(t, resp), &out))
	got := map[string][]string{}
	for _, m := range out.Data {
		got[m.ID] = m.SupportedOperations
	}
	require.Equal(t, map[string][]string{"fast": nil, "cover": {"images.generations"}}, got)
}
