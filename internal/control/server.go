// Package control exposes a minimal HTTP API over a Unix domain socket for nyxd.
// The companion CLI is cmd/nyx (binary: nyx). API contract: docs/openapi.yaml.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zrougamed/nyxd/internal/image"
	"github.com/zrougamed/nyxd/internal/network"
	"github.com/zrougamed/nyxd/internal/runtime"
	"github.com/zrougamed/nyxd/internal/supervisor"
)

// mimeExecStreamV1 request body: one JSON line {"argv":[...]}\n then bytes streamed to
// crun exec stdin until the client closes the body (nyx exec -i).
const mimeExecStreamV1 = "application/x-nyxd-exec+v1"

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
	sup       *supervisor.Supervisor
	dataDir   string // e.g. /var/lib/nyxd (for container log files)
	baseCtx   context.Context
	version   string
	gitCommit string
	buildDate string
	socket      string
	socketGroup string // optional: chgrp socket for non-root nyx clients (0660)

	srv    *http.Server
	ln     net.Listener
	execMu sync.Mutex
}

// New constructs a control server. sup may be nil (run returns 503). store may be nil (pull 503).
// baseCtx should be the daemon lifetime context (e.g. signal-notify ctx) for supervisor.Start.
// lister is used for GET /v1/containers; if nil but sup non-nil, sup is used as Lister.
// dataDir is the daemon base directory (logs live under dataDir/logs).
// socketGroup is optional (e.g. "nyxd"); when set, the socket is chown root:group and mode 0660
// so members of that POSIX group can connect without sudo.
func New(log *slog.Logger, rt *runtime.Runtime, store *image.Store, sup *supervisor.Supervisor, baseCtx context.Context, lister Lister, version, commit, date, dataDir, socket, socketGroup string) *Server {
	l := lister
	if l == nil && sup != nil {
		l = sup
	}
	return &Server{
		log: log, rt: rt, store: store, lister: l, sup: sup, dataDir: dataDir, baseCtx: baseCtx,
		version: version, gitCommit: commit, buildDate: date,
		socket:      socket,
		socketGroup: strings.TrimSpace(socketGroup),
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
	if s.socketGroup != "" {
		grp, err := user.LookupGroup(s.socketGroup)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("control socket group %q: %w", s.socketGroup, err)
		}
		gid, err := strconv.Atoi(grp.Gid)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("control socket group gid: %w", err)
		}
		if err := os.Chown(s.socket, 0, gid); err != nil {
			_ = ln.Close()
			return fmt.Errorf("control socket chown: %w", err)
		}
		s.log.Info("control API socket group", "group", s.socketGroup, "gid", gid)
	}
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.handlePing)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("GET /v1/containers", s.handleContainers)
	mux.HandleFunc("POST /v1/containers/{id}/exec", s.handleExec)
	mux.HandleFunc("POST /v1/containers/{id}/stop", s.handleContainerStop)
	mux.HandleFunc("POST /v1/containers/{id}/remove", s.handleContainerRemove)
	mux.HandleFunc("POST /v1/containers/{id}/kill", s.handleContainerKill)
	mux.HandleFunc("GET /v1/containers/{id}/logs", s.handleContainerLogs)
	mux.HandleFunc("POST /v1/containers/run", s.handleContainerRun)
	mux.HandleFunc("POST /v1/images/pull", s.handleImagePull)
	mux.HandleFunc("GET /v1/images", s.handleImagesList)
	mux.HandleFunc("POST /v1/images/remove", s.handleImagesRemove)
	mux.HandleFunc("POST /v1/images/prune", s.handleImagesPrune)

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
	detail := r.URL.Query().Get("detail") == "1" || r.URL.Query().Get("detail") == "true"

	var ids []string
	if s.lister != nil {
		ids = s.lister.List()
	}

	if detail && s.sup != nil {
		items := s.sup.ListInfo(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"containers": ids,
			"items":      items,
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"containers": ids})
}

type execRequest struct {
	Argv []string `json:"argv"`
}

type flushWriter struct{ http.ResponseWriter }

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.ResponseWriter.Write(p)
	if f, ok := fw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func readExecJSONLine(br *bufio.Reader, max int) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if len(line) > max {
		return nil, fmt.Errorf("exec header line exceeds %d bytes", max)
	}
	s := bytes.TrimSpace(line)
	if len(s) == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return nil, fmt.Errorf("empty exec header line")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return s, nil
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing container id", http.StatusBadRequest)
		return
	}
	canon, err := s.resolveContainerID(id)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	id = canon

	var body execRequest
	var stdin io.Reader

	ct := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Type")))
	switch {
	case ct == mimeExecStreamV1:
		br := bufio.NewReader(r.Body)
		hdr, err := readExecJSONLine(br, 1<<20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(hdr, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stdin = br
	default:
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if len(body.Argv) == 0 {
		http.Error(w, "empty argv", http.StatusBadRequest)
		return
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	fw := &flushWriter{ResponseWriter: w}
	if err := s.rt.Exec(r.Context(), id, body.Argv, stdin, fw, fw); err != nil {
		s.log.Warn("control exec", "id", id, "err", err)
	}
}

func (s *Server) handleContainerStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sup == nil {
		http.Error(w, "supervisor not available", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing container id", http.StatusBadRequest)
		return
	}
	canon, err := s.resolveContainerID(id)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	id = canon
	if err := s.sup.Stop(r.Context(), id); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			http.Error(w, msg, http.StatusNotFound)
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

func (s *Server) handleContainerRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sup == nil {
		http.Error(w, "supervisor not available", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing container id", http.StatusBadRequest)
		return
	}
	canon, err := s.resolveContainerID(id)
	if err != nil {
		writeResolveError(w, err)
		return
	}
	id = canon
	if err := s.sup.Remove(r.Context(), id); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			http.Error(w, msg, http.StatusNotFound)
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

type pullRequest struct {
	Ref    string `json:"ref"`
	Stream bool   `json:"stream,omitempty"`
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

	if !body.Stream {
		cfg, err := s.store.PullWithProgress(r.Context(), body.Ref, nil)
		if err != nil {
			s.log.Warn("pull failed", "ref", body.Ref, "err", err)
			http.Error(w, image.HumanizePullError(err), http.StatusBadGateway)
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
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, ok := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(ev image.PullEvent) {
		if err := enc.Encode(ev); err != nil {
			s.log.Warn("pull stream encode", "err", err)
			return
		}
		if ok {
			fl.Flush()
		}
	}

	cfg, err := s.store.PullWithProgress(r.Context(), body.Ref, emit)
	if err != nil {
		s.log.Warn("pull stream failed", "ref", body.Ref, "err", err)
		_ = enc.Encode(image.PullEvent{Phase: "error", Message: image.HumanizePullError(err)})
		if ok {
			fl.Flush()
		}
		return
	}

	_ = enc.Encode(image.PullEvent{
		Phase:  "done",
		OK:     true,
		RefOut: body.Ref,
		Config: image.SummaryFromConfig(cfg),
	})
	if ok {
		fl.Flush()
	}
}

type runRequest struct {
	ID       string   `json:"id,omitempty"`
	Image    string   `json:"image"`
	Args     []string `json:"args,omitempty"`
	Env      []string `json:"env,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	Restart  string   `json:"restart,omitempty"`
	Publish  []string `json:"publish,omitempty"`
	// Stream requests application/x-ndjson: same PullEvent lines as /v1/images/pull when an
	// image pull is required, then a pull "done" summary when a pull occurred, then a
	// terminal {"phase":"run",...} line with container_id.
	Stream bool `json:"stream,omitempty"`
	Ports  []struct {
		HostPort        int    `json:"hostPort"`
		ContainerPort   int    `json:"containerPort"`
		Protocol        string `json:"protocol,omitempty"`
	} `json:"ports,omitempty"`
}

func (s *Server) handleContainerRun(w http.ResponseWriter, r *http.Request) {
	if s.sup == nil {
		http.Error(w, "supervisor not available", http.StatusServiceUnavailable)
		return
	}
	if s.store == nil {
		http.Error(w, "image store not available", http.StatusServiceUnavailable)
		return
	}
	baseCtx := s.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	var body runRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	imgRef := strings.TrimSpace(body.Image)
	if imgRef == "" {
		http.Error(w, "missing image", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = generateContainerID(imgRef)
	}

	stream := body.Stream
	var streamEnc *json.Encoder
	var streamFlush http.Flusher
	streamStarted := false
	writeStream := func(ev image.PullEvent) {
		if !stream {
			return
		}
		if !streamStarted {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			streamEnc = json.NewEncoder(w)
			if fl, ok := w.(http.Flusher); ok {
				streamFlush = fl
			}
			streamStarted = true
		}
		_ = streamEnc.Encode(ev)
		if streamFlush != nil {
			streamFlush.Flush()
		}
	}
	writeStreamErr := func(msg string) {
		if !stream {
			return
		}
		writeStream(image.PullEvent{Phase: "error", Message: msg})
	}

	var portMaps []network.PortMapping
	for _, pub := range body.Publish {
		p, err := network.ParseDockerPublish(pub)
		if err != nil {
			http.Error(w, "publish: "+err.Error(), http.StatusBadRequest)
			return
		}
		portMaps = append(portMaps, p)
	}
	for _, p := range body.Ports {
		proto := strings.ToLower(strings.TrimSpace(p.Protocol))
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			http.Error(w, "ports: invalid protocol "+proto, http.StatusBadRequest)
			return
		}
		if p.HostPort < 1 || p.HostPort > 65535 || p.ContainerPort < 1 || p.ContainerPort > 65535 {
			http.Error(w, "ports: invalid port range", http.StatusBadRequest)
			return
		}
		portMaps = append(portMaps, network.PortMapping{
			HostPort:        p.HostPort,
			ContainerPort:   p.ContainerPort,
			Protocol:        proto,
		})
	}

	m, cfg, paths, err := s.store.ResolvePulledImage(imgRef)
	if err != nil {
		s.log.Info("pulling image for run", "ref", imgRef, "err", err)
		var pullCb func(image.PullEvent)
		if stream {
			pullCb = func(ev image.PullEvent) { writeStream(ev) }
		}
		if _, errP := s.store.PullWithProgress(baseCtx, imgRef, pullCb); errP != nil {
			s.log.Warn("run: pull failed", "ref", imgRef, "err", errP)
			msg := image.HumanizePullError(errP)
			if stream {
				writeStreamErr(msg)
				return
			}
			http.Error(w, msg, http.StatusBadGateway)
			return
		}
		m, cfg, paths, err = s.store.ResolvePulledImage(imgRef)
		if err != nil {
			s.log.Warn("run: resolve after pull failed", "ref", imgRef, "err", err)
			msg := image.HumanizePullError(err)
			if stream {
				writeStreamErr(msg)
				return
			}
			http.Error(w, msg, http.StatusBadGateway)
			return
		}
		if stream {
			writeStream(image.PullEvent{
				Phase:  "done",
				OK:     true,
				RefOut: imgRef,
				Config: image.SummaryFromConfig(cfg),
			})
		}
	}

	spec := supervisor.ContainerSpec{
		ID:             id,
		Image:          imgRef,
		ImageConfig:    cfg,
		ManifestLayers: m.Layers,
		BlobPaths:      paths,
		Env:            body.Env,
		Args:           body.Args,
		Hostname:       body.Hostname,
		RestartPolicy:  parseRestartPolicy(body.Restart),
		ReadOnly:       false,
		PortMappings:   portMaps,
	}
	if err := s.sup.Start(baseCtx, spec); err != nil {
		s.log.Warn("run: start failed", "id", id, "image", imgRef, "err", err)
		msg := image.TrimUserMessage(err)
		if stream {
			writeStreamErr(msg)
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	if stream {
		writeStream(image.PullEvent{Phase: "run", OK: true, ContainerID: id, Ref: imgRef})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"id":    id,
		"image": imgRef,
	})
}

func parseRestartPolicy(s string) supervisor.RestartPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "unless-stopped", "unless_stopped":
		return supervisor.RestartUnlessStopped
	case "always":
		return supervisor.RestartAlways
	case "on-failure", "on_failure":
		return supervisor.RestartOnFailure
	case "no", "never":
		return supervisor.RestartNever
	default:
		return supervisor.RestartNever
	}
}

func generateContainerID(image string) string {
	var b strings.Builder
	for _, r := range image {
		switch r {
		case '/', ':', '@', '.':
			b.WriteByte('-')
		default:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 48 {
		base = base[:48]
	}
	if base == "" {
		base = "c"
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano()%1e9)
}
