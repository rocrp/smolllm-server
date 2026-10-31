package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoad_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  bind: 0.0.0.0:11435
  access_key: rocry
  env_file: ~/.env.smolllm
  log_level: debug

aliases:
  fast: cerebras/qwen-3,groq/qwen3-32b
  translator: ollama/hy-mt:latest
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "0.0.0.0:11435", cfg.Server.Bind)
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

// `!effort` is smolllm-server config syntax: the server parses it out of each
// leg and hands smolllm-go a per-leg override, so a chain that mixes efforts
// loads exactly as written.
func TestLoad_AcceptsPerLegEffortSuffix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path,
		[]byte("server:\n  access_key: k\naliases:\n  fast: \"groq/qwen3-32b!low,gemini/flash,gemini/pro!none\"\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "groq/qwen3-32b!low,gemini/flash,gemini/pro!none", cfg.ResolveModel("fast"),
		"the alias keeps its suffixes; the adapter strips them per leg")
}

// smolllm-go keys per-leg overrides by spec, so one spec cannot run at two
// efforts. Catching it at load beats discovering it as changed routing.
func TestLoad_RejectsOneSpecWithTwoEfforts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path,
		[]byte("server:\n  access_key: k\naliases:\n  fast: \"gemini/pro!high,gemini/pro!low\"\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), `alias "fast"`)
	require.Contains(t, err.Error(), "two different efforts")
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

func TestStore_ReloadSwapsAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  access_key: v1\naliases:\n  fast: a/b\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	store := NewStore(path, cfg)
	require.Equal(t, "v1", store.Get().Server.AccessKey)
	require.Equal(t, "a/b", store.Get().ResolveModel("fast"))

	// Mutate config on disk and reload.
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  access_key: v2\naliases:\n  fast: c/d\n  slow: e/f\n"), 0o600))

	newCfg, oldCfg, err := store.Reload()
	require.NoError(t, err)
	require.Equal(t, "v1", oldCfg.Server.AccessKey)
	require.Equal(t, "v2", newCfg.Server.AccessKey)
	require.Equal(t, "v2", store.Get().Server.AccessKey)
	require.Equal(t, "c/d", store.Get().ResolveModel("fast"))
	require.Equal(t, "e/f", store.Get().ResolveModel("slow"))
}

func TestStore_ReloadInvalidKeepsCurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(
		"server:\n  access_key: good\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	store := NewStore(path, cfg)

	// Write invalid config (missing access_key).
	require.NoError(t, os.WriteFile(path, []byte("aliases:\n  x: a/b\n"), 0o600))

	_, _, err = store.Reload()
	require.Error(t, err)
	require.Equal(t, "good", store.Get().Server.AccessKey, "stale config retained on error")
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

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return Load(path)
}

func TestLoad_NamedKeysAlone(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, "server:\n  access_keys:\n    laptop: k1\n    rctlsr: k2\n")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"laptop": "k1", "rctlsr": "k2"}, cfg.Keys())
}

func TestLoad_NamedKeysBesideAccessKeyAreTheUnion(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, "server:\n  access_key: k0\n  access_keys:\n    rctlsr: k2\n")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"default": "k0", "rctlsr": "k2"}, cfg.Keys())
}

func TestLoad_RejectsBadNamedKeys(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ yaml, want string }{
		"one key under two names":        {"server:\n  access_keys:\n    a: same\n    b: same\n", "a and b share one key"},
		"named key equals access_key":    {"server:\n  access_key: same\n  access_keys:\n    b: same\n", "b and default share one key"},
		"name default beside access_key": {"server:\n  access_key: k0\n  access_keys:\n    default: k1\n", "clashes with server.access_key"},
		"empty key":                      {"server:\n  access_keys:\n    a: \"\"\n", "server.access_keys.a must not be empty"},
		"name with space":                {"server:\n  access_keys:\n    \"a b\": k\n", `name "a b"`},
		"no key at all":                  {"server:\n  access_keys: {}\n", "server.access_key or server.access_keys"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := load(t, tc.yaml)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoad_ImageAlias(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `
server:
  access_key: k
aliases:
  fast: foo/bar
image_aliases:
  cover: smolayer/codex/gpt-image-2
`))
	require.NoError(t, err)
	model, ok := cfg.ImageModel("cover")
	require.True(t, ok)
	require.Equal(t, "smolayer/codex/gpt-image-2", model)
	_, ok = cfg.ImageModel("fast")
	require.False(t, ok)
	require.Equal(t, 330*time.Second, cfg.Server.ImageTimeout)
}

func TestLoad_ImageTimeoutIsConfigurable(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, "server:\n  access_key: k\n  image_timeout: 5m\n"))
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, cfg.Server.ImageTimeout)

	_, err = Load(writeConfig(t, "server:\n  access_key: k\n  image_timeout: -1s\n"))
	require.ErrorContains(t, err, "server.image_timeout")
}

func TestLoad_ImageAliasRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, spec, alias, want string }{
		{"two legs", "smolayer/codex/gpt-image-2,smolayer/codex/gpt-image-3", "", "exactly one leg"},
		{"non-smolayer leg", "openai/gpt-image-1", "", "smolayer"},
		{"bare model", "gpt-image-2", "", "smolayer"},
		{"effort suffix", "smolayer/codex/gpt-image-2!low", "", "effort"},
		{"empty model", "smolayer/", "", "smolayer"},
		{"name used by a chat alias", "smolayer/codex/gpt-image-2", "cover: foo/bar", "cover"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			aliases := "aliases:\n  x: foo/bar\n"
			if tt.alias != "" {
				aliases = "aliases:\n  " + tt.alias + "\n"
			}
			_, err := Load(writeConfig(t, "server:\n  access_key: k\n"+aliases+"image_aliases:\n  cover: "+tt.spec+"\n"))
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, "image_aliases")
		})
	}
}
