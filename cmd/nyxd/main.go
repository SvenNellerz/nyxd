// nyxd - minimal OCI container orchestrator for NyxOS.
// No Docker, no Podman, no containerd. Just crun + CNI + Go.
package main

import (
	"time"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cerberusos/nyxd/internal/image"
	"github.com/cerberusos/nyxd/internal/log"
	"github.com/cerberusos/nyxd/internal/network"
	"github.com/cerberusos/nyxd/internal/overlay"
	"github.com/cerberusos/nyxd/internal/runtime"
	"github.com/cerberusos/nyxd/internal/supervisor"
)

// Build-time variables injected via -ldflags.
var (
	version   = "dev"
	gitCommit = "unknown"
	buildDate = "unknown"
)

// Config holds daemon configuration.
type Config struct {
	BaseDir    string
	CrunBin    string
	CNIBinDir  string
	CNIConfDir string
	NetworkName string
	LogLevel   string
	Version    bool
}

func main() {
	cfg := parseFlags()

	if cfg.Version {
		fmt.Printf("nyxd %s (commit=%s built=%s)\n", version, gitCommit, buildDate)
		os.Exit(0)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("nyxd starting", "version", version, "baseDir", cfg.BaseDir)

	if os.Geteuid() != 0 {
		log.Error("nyxd must run as root (overlayfs + namespace setup requires it)")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("daemon error", "err", err)
		os.Exit(1)
	}
	log.Info("nyxd stopped cleanly")
}

func run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	// ── Image store ────────────────────────────────────────────────────────────
	imgStore, err := image.NewStore(cfg.BaseDir + "/images")
	if err != nil {
		return fmt.Errorf("image store: %w", err)
	}
	_ = imgStore // used by compose loader / CLI

	// ── Overlay manager ────────────────────────────────────────────────────────
	ovl, err := overlay.NewManager(cfg.BaseDir + "/overlay")
	if err != nil {
		return fmt.Errorf("overlay: %w", err)
	}

	// ── Network manager ────────────────────────────────────────────────────────
	net := network.NewManager(cfg.NetworkName, cfg.CNIConfDir, cfg.CNIBinDir)
	if err := net.EnsureNetwork(); err != nil {
		return fmt.Errorf("cni network setup: %w", err)
	}

	// ── OCI runtime (crun) ─────────────────────────────────────────────────────
	rt, err := runtime.New(cfg.CrunBin, cfg.BaseDir+"/run/crun")
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	// ── Log collector ──────────────────────────────────────────────────────────
	_, err = log.NewCollector(cfg.BaseDir+"/logs", logger)
	if err != nil {
		return fmt.Errorf("log collector: %w", err)
	}

	// ── Supervisor ─────────────────────────────────────────────────────────────
	sup := supervisor.New(rt, ovl, net, cfg.BaseDir, logger)

	// ── Example: pull and run alpine ──────────────────────────────────────────
	// In production this is driven by the compose loader or gRPC API.
	// Remove this block and hook up your compose parser or API server.
	logger.Info("daemon ready - awaiting workload")

	// Demo: you would call sup.Start(ctx, spec) here from your compose/API layer.
	_ = sup

	// ── Wait for shutdown signal ───────────────────────────────────────────────
	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sup.Shutdown(shutdownCtx)

	return nil
}

func parseFlags() Config {
	cfg := Config{}
	flag.StringVar(&cfg.BaseDir, "base-dir", "/var/lib/nyxd", "Base data directory")
	flag.StringVar(&cfg.CrunBin, "crun", "crun", "Path to crun binary")
	flag.StringVar(&cfg.CNIBinDir, "cni-bin-dir", "/opt/cni/bin", "CNI plugin binaries directory")
	flag.StringVar(&cfg.CNIConfDir, "cni-conf-dir", "/etc/cni/net.d", "CNI config directory")
	flag.StringVar(&cfg.NetworkName, "network", "nyx", "CNI network name")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "Log level: debug|info|warn|error")
	flag.BoolVar(&cfg.Version, "version", false, "Print version and exit")
	flag.Parse()
	return cfg
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
