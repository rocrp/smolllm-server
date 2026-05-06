package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  bind: 127.0.0.1:11435
  access_key: rocry
  env_file: ~/.env.smolllm
  log_level: debug

aliases:
  fast: cerebras/qwen-3,groq/qwen3-32b
  translator: ollama/hy-mt:latest
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:11435", cfg.Server.Bind)
	require.Equal(t, "rocry", cfg.Server.AccessKey)
	require.Equal(t, "debug", cfg.Server.LogLevel)
	require.Equal(t, "cerebras/qwen-3,groq/qwen3-32b", cfg.Aliases["fast"])
	require.Equal(t, "ollama/hy-mt:latest", cfg.ResolveModel("translator"))
	require.Equal(t, "openai/gpt-4o-mini", cfg.ResolveModel("openai/gpt-4o-mini")) // passthrough
}

func TestLoad_DefaultsApplied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  access_key: somekey\naliases:\n  x: foo/bar\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, DefaultBind, cfg.Server.Bind)
	require.Equal(t, "somekey", cfg.Server.AccessKey)
	require.Equal(t, DefaultLogLevel, cfg.Server.LogLevel)
}

func TestLoad_AccessKeyRequired(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("aliases:\n  x: foo/bar\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.access_key")
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load("/nonexistent/path/config.yaml")
	require.Error(t, err)
}

func TestLoad_InvalidBind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  bind: not-a-host\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.bind")
}

func TestLoad_InvalidAlias(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  access_key: k\naliases:\n  bad: \"a,,b\"\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty entry")
}

func TestEnvAccessKeyOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  access_key: yamlkey\n"), 0o600))

	t.Setenv(EnvAccessKey, "envkey")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "envkey", cfg.Server.AccessKey)
}

func TestExpandHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		in, want string
	}{
		{"~", home},
		{"~/foo", filepath.Join(home, "foo")},
		{"/abs/path", "/abs/path"},
		{"", ""},
	}
	for _, tc := range tests {
		got, err := expandHome(tc.in)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
}

func TestResolvePath_Order(t *testing.T) {
	// Explicit > env > default. We test the env variant; default is hard to
	// test in isolation because it depends on $HOME.
	t.Setenv(EnvConfigPath, "/tmp/from-env.yaml")
	got, err := ResolvePath("")
	require.NoError(t, err)
	require.Equal(t, "/tmp/from-env.yaml", got)

	got, err = ResolvePath("/tmp/explicit.yaml")
	require.NoError(t, err)
	require.Equal(t, "/tmp/explicit.yaml", got)
}

func TestLoadEnvFile_LoadsKeysWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.smolllm")
	require.NoError(t, os.WriteFile(envPath, []byte("FOO_API_KEY=fromfile\nBAR_BASE_URL=http://x\n"), 0o600))

	cfg := &Config{Server: ServerConfig{EnvFile: envPath}}

	t.Setenv("FOO_API_KEY", "preexisting")
	require.NoError(t, cfg.LoadEnvFile())
	require.Equal(t, "preexisting", os.Getenv("FOO_API_KEY")) // not overwritten
	require.Equal(t, "http://x", os.Getenv("BAR_BASE_URL"))   // newly set
}
