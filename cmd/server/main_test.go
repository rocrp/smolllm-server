package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/server"
	"github.com/stretchr/testify/require"
)

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

const twoKeys = "server:\n  access_keys:\n    laptop: k-laptop\n    rctlsr: k-rctlsr\n"

// The whole path a revocation takes: the file is edited, the watcher reloads
// it, and the next request with the removed key is refused.
func TestNamedKeys_HotReloadRevokesWithinASecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(twoKeys), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	store := config.NewStore(path, cfg)
	srv := server.New(store, logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go watchConfig(ctx, store, srv, new(slog.LevelVar), logger)
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "config auto-reload enabled") },
		time.Second, 10*time.Millisecond)

	status := func(key string) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, status("k-laptop"))
	require.Equal(t, http.StatusOK, status("k-rctlsr"))
	require.Equal(t, http.StatusUnauthorized, status("k-other"))

	var rctlsrLine string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "msg=request ") && strings.Contains(line, "key=rctlsr") {
			rctlsrLine = line
		}
	}
	require.NotEmpty(t, rctlsrLine, "a request line names the key:\n%s", logs.String())
	require.NotContains(t, logs.String(), "k-rctlsr", "the key itself never reaches the log")

	require.NoError(t, os.WriteFile(path, []byte("server:\n  access_keys:\n    laptop: k-laptop\n"), 0o600))
	require.Eventually(t, func() bool { return status("k-rctlsr") == http.StatusUnauthorized },
		time.Second, 20*time.Millisecond, "the removed key is refused within a second")
	require.Equal(t, http.StatusOK, status("k-laptop"))
}

// A key rotation as the secrets sync performs it: the env file, kept in its
// own directory, is replaced by an atomic rename and the new value is live
// within a second without touching config.yaml.
func TestEnvFileRotation_HotReloadsProviderKey(t *testing.T) {
	const name = "ROTATION_TEST_API_KEY"
	t.Setenv(name, "")
	envPath := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(envPath, []byte(name+"=old\n"), 0o600))
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(twoKeys+"  env_file: "+envPath+"\n"), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NoError(t, cfg.ReloadEnvFile())
	require.Equal(t, "old", os.Getenv(name))

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	store := config.NewStore(path, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go watchConfig(ctx, store, server.New(store, logger), new(slog.LevelVar), logger)
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "config auto-reload enabled") },
		time.Second, 10*time.Millisecond)

	staged := envPath + ".tmp"
	require.NoError(t, os.WriteFile(staged, []byte(name+"=new\n"), 0o600))
	require.NoError(t, os.Rename(staged, envPath))
	require.Eventually(t, func() bool { return os.Getenv(name) == "new" },
		time.Second, 20*time.Millisecond, "the rotated key is live within a second:\n%s", logs.String())
}
