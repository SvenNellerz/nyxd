// nyxd - minimal OCI container orchestrator for NyxOS.
// No Docker, no Podman, no containerd. Just crun + CNI + Go.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zrougamed/nyxd/internal/control"
	"github.com/zrougamed/nyxd/internal/image"
	"github.com/zrougamed/nyxd/internal/logs"
	"github.com/zrougamed/nyxd/internal/network"
	"github.com/zrougamed/nyxd/internal/overlay"
	"github.com/zrougamed/nyxd/internal/runtime"
	"github.com/zrougamed/nyxd/internal/supervisor"
)

// Build-time variables injected via -ldflags.
var (
	version   = "dev"
	gitCommit = "unknown"
	buildDate = "unknown"
)

// Config holds daemon configuration.
type Config struct {
	BaseDir     string
	CrunBin     string
	CNIBinDir   string
	CNIConfDir  string
	NetworkName string
	LogLevel    string
	Version     bool
	Socket      string // Unix socket for HTTP control API; empty disables
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
	imgStore, err := image.NewStore(cfg.BaseDir + "/images")
	if err != nil {
		return fmt.Errorf("image store: %w", err)
	}

	ovl, err := overlay.NewManager(cfg.BaseDir + "/overlay")
	if err != nil {
		return fmt.Errorf("overlay: %w", err)
	}

	net := network.NewManager(cfg.NetworkName, cfg.CNIConfDir, cfg.CNIBinDir)
	if err := net.EnsureNetwork(); err != nil {
		return fmt.Errorf("cni network setup: %w", err)
	}

	rt, err := runtime.New(cfg.CrunBin, cfg.BaseDir+"/run/crun")
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	_, err = logs.NewCollector(cfg.BaseDir+"/logs", logger)
	if err != nil {
		return fmt.Errorf("log collector: %w", err)
	}

	sup := supervisor.New(rt, ovl, net, cfg.BaseDir, logger)

	ctl := control.New(logger, rt, imgStore, sup, ctx, nil, version, gitCommit, buildDate, cfg.Socket)
	if err := ctl.Start(); err != nil {
		logger.Warn("control API not started", "err", err)
	} else {
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := ctl.Shutdown(sctx); err != nil {
				logger.Warn("control API shutdown", "err", err)
			}
		}()
	}

	logger.Info("daemon ready - awaiting workload")

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
	flag.StringVar(&cfg.Socket, "socket", "/run/nyxd/nyxd.sock", "Unix socket for HTTP control API (nyx client); set to \"\" to disable")
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
