// Package control exposes a minimal HTTP API over a Unix domain socket for nyxd.
// The companion CLI is cmd/nyx (binary: nyx). Intended to grow into the OpenAPI-backed surface.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zrougamed/nyxd/internal/image"
	"github.com/zrougamed/nyxd/internal/runtime"
)

// Lister optionally lists supervised container IDs.
type Lister interface {
	List() []string
}

// Server serves HTTP on a Unix socket (e.g. /run/nyxd/nyxd.sock).
type Server struct {
	log       *slog.Logger
	rt        *runtime.Runtime
	store     *image.Store
	lister    Lister
	version   string
	gitCommit string
	buildDate string
	socket    string

	srv     *http.Server
	ln      net.Listener
	execMu  sync.Mutex
}

// New constructs a control server. lister may be nil (containers route returns empty).
// store may be nil (image pull returns 503). socket empty disables listening.
func New(log *slog.Logger, rt *runtime.Runtime, store *image.Store, lister Lister, version, commit, date, socket string) *Server {
	return &Server{
		log: log, rt: rt, store: store, lister: lister,
		version: version, gitCommit: commit, buildDate: date,
		socket: socket,
	}
}

// Start binds the Unix socket and serves HTTP until Shutdown is called.
func (s *Server) Start() error {
	if s.socket == "" {
		s.log.Info("control API disabled", "reason", "empty -socket path")
		return nil
	}
	dir := filepath.Dir(s.socket)
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return fmt.Errorf("control socket dir: %w", err)
	}
	_ = os.Remove(s.socket)
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("control listen %s: %w", s.socket, err)
	}
	if err := os.Chmod(s.socket, 0o660); err != nil {
		_ = ln.Close()
		return fmt.Errorf("control socket chmod: %w", err)
	}
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.handlePing)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("GET /v1/containers", s.handleContainers)
	mux.HandleFunc("POST /v1/containers/{id}/exec", s.handleExec)
	mux.HandleFunc("POST /v1/images/pull", s.handleImagePull)

	s.srv = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 15 * time.Minute,
	}
	go func() {
		s.log.Info("control API listening", "socket", s.socket)
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("control server", "err", err)
		}
	}()
	return nil
}

// Shutdown stops the HTTP server and removes the socket path.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.socket != "" {
		_ = os.Remove(s.socket)
	}
	return err
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"daemon":    "nyxd",
		"version":   s.version,
		"commit":    s.gitCommit,
		"buildDate": s.buildDate,
	})
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var ids []string
	if s.lister != nil {
		ids = s.lister.List()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"containers": ids})
}

type execRequest struct {
	Argv []string `json:"argv"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing container id", http.StatusBadRequest)
		return
	}
	var body execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Argv) == 0 {
		http.Error(w, "empty argv", http.StatusBadRequest)
		return
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := s.rt.Exec(r.Context(), id, body.Argv, w, w); err != nil {
		s.log.Warn("control exec", "id", id, "err", err)
	}
}

type pullRequest struct {
	Ref string `json:"ref"`
}

func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "image store not configured", http.StatusServiceUnavailable)
		return
	}
	var body pullRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Ref) == "" {
		http.Error(w, "missing ref", http.StatusBadRequest)
		return
	}
	cfg, err := s.store.Pull(r.Context(), body.Ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":  true,
		"ref": body.Ref,
		"config": map[string]any{
			"os":             cfg.OS,
			"architecture":   cfg.Architecture,
			"entrypoint":     cfg.Config.Entrypoint,
			"cmd":            cfg.Config.Cmd,
			"working_dir":    cfg.Config.WorkingDir,
			"env_len":        len(cfg.Config.Env),
		},
	})
}
