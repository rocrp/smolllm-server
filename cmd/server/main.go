package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
		"access_key", maskKey(cfg.Server.AccessKey),
		"env_file", cfg.Server.EnvFile,
	)

	srv := server.New(store, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Auto-reload on file change. For bind changes (or any change you want
	// to force-apply immediately even if the file didn't move), use
	// `make reload`, which restarts the process.
	go watchConfig(ctx, store, srv, levelVar, logger)

	return srv.Run(ctx)
}

// watchConfig auto-reloads when the config file changes on disk.
//
// We watch the parent directory because editors typically save atomically
// (write tmp → rename over original); a watch bound to the original inode
// would go stale after such a save. Filtering by basename inside the
// directory catches both atomic renames and in-place writes.
//
// A 200 ms debounce coalesces editor bursts (chmod + write + rename) into
// a single reload.
func watchConfig(ctx context.Context, store *config.Store, srv *server.Server, levelVar *slog.LevelVar, logger *slog.Logger) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("fsnotify init failed; auto-reload disabled", "error", err)
		return
	}
	defer w.Close()

	dir, base := filepath.Split(store.Path())
	if err := w.Add(dir); err != nil {
		logger.Warn("fsnotify add failed; auto-reload disabled", "dir", dir, "error", err)
		return
	}
	logger.Info("config auto-reload enabled", "watching", store.Path())

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
			if filepath.Base(ev.Name) != base {
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
		logger.Warn("env file reload failed", "error", err)
	}

	logger.Info("config reloaded",
		"path", store.Path(),
		"aliases", aliasNames(newCfg.Aliases),
		"access_key", maskKey(newCfg.Server.AccessKey),
		"log_level", newCfg.Server.LogLevel,
	)
	if newCfg.Server.Bind != srv.Bind() {
		logger.Warn("server.bind changed but cannot hot-reload; run `make reload` to re-bind",
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

func maskKey(k string) string {
	if len(k) <= 2 {
		return "***"
	}
	return string(k[0]) + strings.Repeat("*", len(k)-2) + string(k[len(k)-1])
}
