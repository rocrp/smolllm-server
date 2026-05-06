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

	logger := newLogger(cfg.Server.LogLevel)
	logger.Info("config loaded",
		"path", cfgPath,
		"bind", cfg.Server.Bind,
		"aliases", aliasNames(cfg.Aliases),
		"access_key", maskKey(cfg.Server.AccessKey),
		"env_file", cfg.Server.EnvFile,
	)

	srv := server.New(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
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
