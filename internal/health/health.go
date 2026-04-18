// Package health implements container healthchecks.
// Supports: exec (runs inside container via crun exec), http, tcp.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// Type identifies the healthcheck kind.
type Type string

const (
	TypeExec Type = "exec"
	TypeHTTP Type = "http"
	TypeTCP  Type = "tcp"
	TypeNone Type = "none"
)

// Config defines how to check container health.
type Config struct {
	Type        Type
	Command     []string      // for exec
	URL         string        // for http: "http://127.0.0.1:8080/health"
	Address     string        // for tcp: "127.0.0.1:8080"
	Interval    time.Duration // time between checks
	Timeout     time.Duration // per-check timeout
	Retries     int           // consecutive failures before unhealthy
	StartPeriod time.Duration // grace period before first check
}

// Status of a container's healthcheck.
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusStarting  Status = "starting"
)

// Checker runs healthchecks for a container and notifies on status change.
type Checker struct {
	containerID string
	cfg         Config
	crunBin     string // path to crun binary for exec checks
	crunRoot    string
	log         *slog.Logger

	mu          sync.RWMutex
	status      Status
	consecutive int // consecutive failures
	onUnhealthy func(containerID string) // called when container becomes unhealthy

	cancel context.CancelFunc
}

// New creates a Checker. onUnhealthy is called (once) when a container fails its healthcheck.
func New(containerID string, cfg Config, crunBin, crunRoot string, log *slog.Logger, onUnhealthy func(string)) *Checker {
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Retries == 0 {
		cfg.Retries = 3
	}
	if cfg.StartPeriod == 0 {
		cfg.StartPeriod = 0
	}
	return &Checker{
		containerID: containerID,
		cfg:         cfg,
		crunBin:     crunBin,
		crunRoot:    crunRoot,
		log:         log,
		status:      StatusStarting,
		onUnhealthy: onUnhealthy,
	}
}

// Start begins running health checks in the background.
// Call Stop to clean up.
func (c *Checker) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	go c.loop(ctx)
}

// Stop halts the health checker.
func (c *Checker) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

// Status returns the current health status.
func (c *Checker) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// loop runs healthchecks on the configured interval.
func (c *Checker) loop(ctx context.Context) {
	// Wait out start period before first check.
	if c.cfg.StartPeriod > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.StartPeriod):
		}
	}

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	// Run once immediately after start period.
	c.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check(ctx)
		}
	}
}

func (c *Checker) check(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var err error
	switch c.cfg.Type {
	case TypeExec:
		err = c.checkExec(checkCtx)
	case TypeHTTP:
		err = c.checkHTTP(checkCtx)
	case TypeTCP:
		err = c.checkTCP(checkCtx)
	case TypeNone, "":
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.consecutive++
		c.log.Warn("healthcheck failed",
			"id", c.containerID,
			"type", c.cfg.Type,
			"err", err,
			"consecutive", c.consecutive,
		)
		if c.consecutive >= c.cfg.Retries && c.status != StatusUnhealthy {
			c.status = StatusUnhealthy
			if c.onUnhealthy != nil {
				go c.onUnhealthy(c.containerID)
			}
		}
	} else {
		if c.consecutive > 0 {
			c.log.Info("healthcheck recovered", "id", c.containerID)
		}
		c.consecutive = 0
		c.status = StatusHealthy
	}
}

// checkExec runs a command inside the container using crun exec.
func (c *Checker) checkExec(ctx context.Context) error {
	if len(c.cfg.Command) == 0 {
		return fmt.Errorf("exec healthcheck: no command")
	}
	// crun exec <id> <cmd...>
	args := []string{"--root", c.crunRoot, "exec", c.containerID}
	args = append(args, c.cfg.Command...)
	cmd := exec.CommandContext(ctx, c.crunBin, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// checkHTTP sends a GET to the configured URL and expects 2xx.
func (c *Checker) checkHTTP(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{} // timeout already on ctx
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http healthcheck: %d", resp.StatusCode)
	}
	return nil
}

// checkTCP dials the configured address and expects a successful connection.
func (c *Checker) checkTCP(ctx context.Context) error {
	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", c.cfg.Address)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}
