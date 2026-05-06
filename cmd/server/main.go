package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

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

	// SIGHUP → hot reload. Aliases, access_key, log_level, and env_file
	// contents take effect immediately. Bind drift is logged and ignored
	// (requires `make reload` to actually re-bind).
	go reloadOnSignal(ctx, store, srv, levelVar, logger)

	return srv.Run(ctx)
}

func reloadOnSignal(ctx context.Context, store *config.Store, srv *server.Server, levelVar *slog.LevelVar, logger *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			doReload(store, srv, levelVar, logger)
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
