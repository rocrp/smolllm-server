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

	// Hot reload triggers:
	//   1. SIGHUP — explicit reload (launchctl/kill -HUP)
	//   2. fsnotify — auto reload when the YAML file changes on disk
	// Both funnel into the same doReload(); aliases, access_key, log_level,
	// and env_file contents take effect immediately. Bind drift is logged
	// and ignored (requires `make reload` to actually re-bind).
	reload := func() { doReload(store, srv, levelVar, logger) }
	go reloadOnSignal(ctx, reload, logger)
	go watchConfigFile(ctx, cfgPath, reload, logger)

	return srv.Run(ctx)
}

func reloadOnSignal(ctx context.Context, reload func(), logger *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			logger.Debug("reload triggered by SIGHUP")
			reload()
		}
	}
}

// watchConfigFile auto-reloads when cfgPath changes on disk.
//
// We watch the *parent directory* rather than the file itself because most
// editors save atomically: write to tmp → rename over original. After such
// a rename, an inode-bound watch on the original file goes stale and
// silently stops firing. Watching the directory and filtering by basename
// catches both atomic renames and in-place writes.
//
// Events are debounced (200 ms) to coalesce the burst that editors emit
// (CHMOD + WRITE + RENAME etc.) into a single reload.
func watchConfigFile(ctx context.Context, cfgPath string, reload func(), logger *slog.Logger) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("fsnotify init failed; auto-reload disabled (SIGHUP still works)", "error", err)
		return
	}
	defer w.Close()

	dir := filepath.Dir(cfgPath)
	base := filepath.Base(cfgPath)
	if err := w.Add(dir); err != nil {
		logger.Warn("fsnotify add failed; auto-reload disabled", "dir", dir, "error", err)
		return
	}
	logger.Info("config auto-reload enabled", "watching", cfgPath)

	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	fire := make(chan struct{}, 1)

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
			// Coalesce bursts. Reset on every relevant event.
			if timer == nil {
				timer = time.AfterFunc(debounce, func() {
					select {
					case fire <- struct{}{}:
					default:
					}
				})
			} else {
				timer.Reset(debounce)
			}

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			logger.Warn("fsnotify error", "error", err)

		case <-fire:
			logger.Debug("reload triggered by file change", "path", cfgPath)
			reload()
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

	// Apply side-effects that are NOT just "read from store on next request":
	//   - log level: backed by slog.LevelVar
	//   - env file: re-source with overload so rotated keys take effect
	if newCfg.Server.LogLevel != oldCfg.Server.LogLevel {
		levelVar.Set(parseLevel(newCfg.Server.LogLevel))
	}
	if err := newCfg.ReloadEnvFile(); err != nil {
		logger.Warn("env file reload failed", "error", err)
	}

	// Bind cannot be hot-changed without dropping the listener.
	bindChanged := newCfg.Server.Bind != srv.Bind()

	logger.Info("config reloaded",
		"path", store.Path(),
		"aliases", aliasNames(newCfg.Aliases),
		"access_key", maskKey(newCfg.Server.AccessKey),
		"log_level", newCfg.Server.LogLevel,
		"env_file", newCfg.Server.EnvFile,
		"access_key_rotated", newCfg.Server.AccessKey != oldCfg.Server.AccessKey,
		"bind_change_ignored", bindChanged,
	)
	if bindChanged {
		logger.Warn("server.bind changed but cannot hot-reload; restart required",
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
