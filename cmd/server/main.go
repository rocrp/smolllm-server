package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		flagConfig = flag.String("config", "", "path to config.yaml (default: $SMOLLLM_SERVER_CONFIG or ~/.config/smolllm-server/config.yaml)")
		flagBind   = flag.String("bind", "", "override server bind address (e.g. 127.0.0.1:11435)")
	)
	flag.Parse()

	cfgPath, err := config.ResolvePath(*flagConfig)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if *flagBind != "" {
		cfg.Server.Bind = *flagBind
		if err := cfg.Validate(); err != nil {
			return err
		}
	}

	if err := cfg.LoadEnvFile(); err != nil {
		return err
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(parseLevel(cfg.Server.LogLevel))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar}))

	store := config.NewStore(cfgPath, cfg)
	logger.Info("config loaded",
		"path", cfgPath,
		"bind", cfg.Server.Bind,
		"aliases", aliasNames(cfg.Aliases),
		"keys", keyNames(cfg.Keys()),
		"env_file", cfg.Server.EnvFile,
	)

	srv := server.New(store, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Shutdown watchdog: if anything stalls after signal arrives (graceful
	// shutdown, deferred Close, runtime hang), force-exit so launchd's
	// KeepAlive respawns us. Without this, a stuck-but-alive process is
	// invisible to launchd and the service silently dies.
	go func() {
		<-ctx.Done()
		time.Sleep(10 * time.Second)
		fmt.Fprintln(os.Stderr, "shutdown watchdog: forcing exit after 10s")
		os.Exit(1)
	}()

	// Auto-reload on file change. For bind changes (or any change you want
	// to force-apply immediately even if the file didn't move), use
	// `just reload`, which restarts the process.
	go watchConfig(ctx, store, srv, levelVar, logger)

	return srv.Run(ctx)
}

// watchConfig auto-reloads when the config file or its env file changes on
// disk, so a rotated provider key lands without a restart.
//
// We watch parent directories because editors and `install` save atomically
// (write tmp → rename over original); a watch bound to the original inode
// would go stale after such a save. Filtering by full path inside each
// directory catches both atomic renames and in-place writes.
//
// The env file path is read once at startup: pointing env_file elsewhere
// needs `just reload`.
//
// A 200 ms debounce coalesces editor bursts (chmod + write + rename) into
// a single reload.
func watchConfig(ctx context.Context, store *config.Store, srv *server.Server, levelVar *slog.LevelVar, logger *slog.Logger) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Error("fsnotify init failed; auto-reload disabled", "error", err)
		return
	}
	defer w.Close()

	watched := []string{filepath.Clean(store.Path())}
	envPath, err := store.Get().EnvFilePath()
	if err != nil {
		logger.Error("resolve env file failed; auto-reload disabled", "error", err)
		return
	}
	if envPath != "" {
		watched = append(watched, filepath.Clean(envPath))
	}
	for _, path := range watched {
		if err := w.Add(filepath.Dir(path)); err != nil {
			logger.Error("fsnotify add failed; auto-reload disabled", "dir", filepath.Dir(path), "error", err)
			return
		}
	}
	logger.Info("config auto-reload enabled", "watching", watched)

	var debounce *time.Timer
	reload := func() { doReload(store, srv, levelVar, logger) }

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if !slices.Contains(watched, filepath.Clean(ev.Name)) {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(200*time.Millisecond, reload)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			logger.Warn("fsnotify error", "error", err)
		}
	}
}

func doReload(store *config.Store, srv *server.Server, levelVar *slog.LevelVar, logger *slog.Logger) {
	newCfg, oldCfg, err := store.Reload()
	if err != nil {
		logger.Error("config reload failed; keeping previous snapshot",
			"path", store.Path(), "error", err)
		return
	}

	if newCfg.Server.LogLevel != oldCfg.Server.LogLevel {
		levelVar.Set(parseLevel(newCfg.Server.LogLevel))
	}
	if err := newCfg.ReloadEnvFile(); err != nil {
		logger.Error("env file reload failed; provider keys unchanged", "error", err)
	}

	logger.Info("config reloaded",
		"path", store.Path(),
		"aliases", aliasNames(newCfg.Aliases),
		"keys", keyNames(newCfg.Keys()),
		"log_level", newCfg.Server.LogLevel,
	)
	if newCfg.Server.Bind != srv.Bind() {
		logger.Warn("server.bind changed but cannot hot-reload; run `just reload` to re-bind",
			"current", srv.Bind(), "config", newCfg.Server.Bind)
	}
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func aliasNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// keyNames lists the access keys by name only: a key never reaches the log.
func keyNames(keys map[string]string) []string {
	return slices.Sorted(maps.Keys(keys))
}
