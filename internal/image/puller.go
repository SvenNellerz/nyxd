// Package image implements a minimal OCI image puller.
// No registry SDK, no Docker client - raw HTTP + OCI distribution spec.
package image

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zrougamed/nyxd/pkg/oci"
)

const (
	defaultRegistry    = "registry-1.docker.io"
	tokenEndpoint      = "https://auth.docker.io/token"
	maxLayerConcurrent = 3
	pullTimeout        = 10 * time.Minute
	layerBufSize       = 32 * 1024
)

// ParsedRef holds normalized components of an image reference.
type ParsedRef struct {
	Registry string
	Repo     string
	Tag      string
	Digest   string
}

// ParseRef parses a container image reference (used by tests and callers).
func ParseRef(input string) (ParsedRef, error) {
	reg, repo, tag := parseRef(input)
	pr := ParsedRef{Registry: reg, Repo: repo}
	if strings.Contains(input, "@") {
		pr.Digest = tag
	} else {
		pr.Tag = tag
	}
	return pr, nil
}

// Store manages OCI blobs and image metadata on disk.
//
//	<storeRoot>/blobs/sha256/<hex>          – raw compressed blobs
//	<storeRoot>/images/<repo>/<tag>/        – manifest.json + config.json
type Store struct {
	root string
	mu   sync.RWMutex
}

// NewStore creates (or opens) a blob store at root.
func NewStore(root string) (*Store, error) {
	for _, sub := range []string{"blobs/sha256", "images"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return nil, fmt.Errorf("image store init: %w", err)
		}
	}
	return &Store{root: root}, nil
}

// Pull fetches an image from a registry and caches blobs locally.
// ref format: [registry/]name[:tag|@digest]
func (s *Store) Pull(ctx context.Context, ref string) (*oci.ImageConfig, error) {
	return s.PullWithProgress(ctx, ref, nil)
}

// PullWithProgress runs Pull and invokes on for each PullEvent (e.g. NDJSON streaming).
// on must be non-blocking or very fast; the caller serializes if needed.
func (s *Store) PullWithProgress(ctx context.Context, ref string, on func(PullEvent)) (*oci.ImageConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	var emitMu sync.Mutex
	emit := func(ev PullEvent) {
		if on == nil {
			return
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		on(ev)
	}

	emit(PullEvent{Phase: "begin", Ref: ref})

	reg, repo, tag := parseRef(ref)
	client := &registryClient{
		registry: reg,
		repo:     repo,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}

	if err := client.auth(ctx); err != nil {
		return nil, fmt.Errorf("pull auth %s: %w", ref, err)
	}

	emit(PullEvent{Phase: "auth", Ref: ref, Message: "ok"})

	manifest, err := client.manifest(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("pull manifest %s: %w", ref, err)
	}

	emit(PullEvent{
		Phase:      "manifest",
		Ref:        ref,
		MediaType:  manifest.MediaType,
		LayerCount: len(manifest.Layers),
	})

	emit(PullEvent{
		Phase:  "config",
		Digest: manifest.Config.Digest,
		Size:   manifest.Config.Size,
	})

	cfgBlob, err := s.fetchBlob(ctx, client, manifest.Config, func(cur int64) {
		emit(PullEvent{Phase: "progress", Digest: manifest.Config.Digest, Current: cur})
	})
	if err != nil {
		return nil, fmt.Errorf("pull config: %w", err)
	}
	var imgCfg oci.ImageConfig
	if err := json.Unmarshal(cfgBlob, &imgCfg); err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}

	emit(PullEvent{Phase: "layer_done", Digest: manifest.Config.Digest})

	if err := s.pullLayers(ctx, client, manifest.Layers, emit); err != nil {
		return nil, fmt.Errorf("pull layers: %w", err)
	}

	emit(PullEvent{Phase: "meta", Ref: ref, Message: "writing image metadata"})

	if err := s.writeImageMeta(ref, manifest, &imgCfg); err != nil {
		return nil, fmt.Errorf("write image meta: %w", err)
	}

	return &imgCfg, nil
}

// BlobPath returns the on-disk path of a blob by digest.
func (s *Store) BlobPath(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(s.root, "blobs", "sha256", hex)
}

// LoadManifest reads an OCI image manifest JSON from the blob store.
func (s *Store) LoadManifest(digest string) (*oci.Manifest, error) {
	data, err := os.ReadFile(s.BlobPath(digest))
	if err != nil {
		return nil, fmt.Errorf("read manifest blob: %w", err)
	}
	var m oci.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// HasBlob returns true if the blob is already cached locally.
func (s *Store) HasBlob(digest string) bool {
	_, err := os.Stat(s.BlobPath(digest))
	return err == nil
}

// LoadImageMeta loads a previously pulled image's manifest + config.
func (s *Store) LoadImageMeta(ref string) (*oci.Manifest, *oci.ImageConfig, error) {
	_, repo, tag := parseRef(ref)
	dir := filepath.Join(s.root, "images", sanitisePath(repo), sanitisePath(tag))

	mData, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("image not found locally (pull first): %w", err)
	}
	var m oci.Manifest
	if err := json.Unmarshal(mData, &m); err != nil {
		return nil, nil, err
	}

	cData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, nil, err
	}
	var cfg oci.ImageConfig
	if err := json.Unmarshal(cData, &cfg); err != nil {
		return nil, nil, err
	}
	return &m, &cfg, nil
}

// ResolvePulledImage returns manifest, config, and on-disk blob paths (base layer first)
// for an image already present in the store (pull first).
func (s *Store) ResolvePulledImage(ref string) (*oci.Manifest, *oci.ImageConfig, []string, error) {
	m, cfg, err := s.LoadImageMeta(ref)
	if err != nil {
		return nil, nil, nil, err
	}
	paths := make([]string, 0, len(m.Layers))
	for _, d := range m.Layers {
		p := s.BlobPath(d.Digest)
		if _, err := os.Stat(p); err != nil {
			return nil, nil, nil, fmt.Errorf("missing layer blob %s: %w", d.Digest, err)
		}
		paths = append(paths, p)
	}
	return m, cfg, paths, nil
}

// ListImageRefs returns pulled image references (repo:tag) discovered on disk.
func (s *Store) ListImageRefs() ([]string, error) {
	imagesRoot := filepath.Join(s.root, "images")
	var out []string
	err := filepath.WalkDir(imagesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() != "manifest.json" {
			return nil
		}
		rel, err := filepath.Rel(imagesRoot, filepath.Dir(path))
		if err != nil {
			return nil
		}
		segs := strings.Split(rel, string(filepath.Separator))
		if len(segs) < 2 {
			return nil
		}
		tag := segs[len(segs)-1]
		repo := strings.Join(segs[:len(segs)-1], "/")
		out = append(out, repo+":"+tag)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// RemoveImage deletes local metadata for ref (manifest + config). Blobs are left in the content store.
func (s *Store) RemoveImage(ref string) error {
	if _, _, err := s.LoadImageMeta(ref); err != nil {
		return err
	}
	_, repo, tag := parseRef(ref)
	dir := filepath.Join(s.root, "images", sanitisePath(repo), sanitisePath(tag))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove image dir: %w", err)
	}
	return nil
}

// pullLayers fetches all layers with bounded concurrency.
func (s *Store) pullLayers(ctx context.Context, client *registryClient, layers []oci.Descriptor, emit func(PullEvent)) error {
	type result struct{ err error }
	results := make(chan result, len(layers))
	sem := make(chan struct{}, maxLayerConcurrent)

	var wg sync.WaitGroup
	for i, layer := range layers {
		cached := s.HasBlob(layer.Digest)
		emit(PullEvent{
			Phase:   "layer",
			Index:   i,
			Count:   len(layers),
			Digest:  layer.Digest,
			Size:    layer.Size,
			Cached:  cached,
		})
		if cached {
			emit(PullEvent{Phase: "layer_done", Digest: layer.Digest})
			continue
		}
		wg.Add(1)
		go func(desc oci.Descriptor) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := s.fetchBlobToDisk(ctx, client, desc, func(cur int64) {
				emit(PullEvent{Phase: "progress", Digest: desc.Digest, Current: cur})
			})
			if err == nil {
				emit(PullEvent{Phase: "layer_done", Digest: desc.Digest})
			}
			results <- result{err}
		}(layer)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var errs []error
	for r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	return errors.Join(errs...)
}

// progressReader wraps an io.Reader to emit absolute byte counts periodically.
type progressReader struct {
	r    io.Reader
	step int64
	next int64
	n    int64
	fn   func(int64)
}

func newProgressReader(r io.Reader, fn func(int64)) io.Reader {
	if fn == nil {
		return r
	}
	const step = 256 * 1024
	if step < 1 {
		return r
	}
	return &progressReader{r: r, step: step, next: step, fn: fn}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n += int64(n)
		if p.fn != nil && p.n >= p.next {
			p.fn(p.n)
			for p.next <= p.n {
				p.next += p.step
			}
		}
	}
	if err == io.EOF && p.fn != nil {
		p.fn(p.n)
	}
	return n, err
}

// fetchBlob downloads a small blob (e.g. config) and returns its bytes.
func (s *Store) fetchBlob(ctx context.Context, client *registryClient, desc oci.Descriptor, onProgress func(int64)) ([]byte, error) {
	dest := s.BlobPath(desc.Digest)

	if data, err := os.ReadFile(dest); err == nil {
		if onProgress != nil {
			onProgress(int64(len(data)))
		}
		return data, nil
	}

	if err := s.fetchBlobToDisk(ctx, client, desc, onProgress); err != nil {
		return nil, err
	}
	return os.ReadFile(dest)
}

// fetchBlobToDisk streams a blob to the content store without holding the full payload in memory.
func (s *Store) fetchBlobToDisk(ctx context.Context, client *registryClient, desc oci.Descriptor, onProgress func(int64)) error {
	dest := s.BlobPath(desc.Digest)
	if st, err := os.Stat(dest); err == nil {
		if onProgress != nil {
			onProgress(st.Size())
		}
		return nil
	}

	rc, err := client.blob(ctx, desc.Digest)
	if err != nil {
		return fmt.Errorf("fetch blob %s: %w", desc.Digest, err)
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	h := sha256.New()
	pr := newProgressReader(rc, onProgress)
	tee := io.TeeReader(pr, h)
	buf := make([]byte, layerBufSize)

	if _, err := io.CopyBuffer(tmp, tee, buf); err != nil {
		cleanup()
		return fmt.Errorf("stream blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	tmp.Close()

	got := fmt.Sprintf("sha256:%x", h.Sum(nil))
	if got != desc.Digest {
		os.Remove(tmpName)
		return fmt.Errorf("digest mismatch: want %s got %s", desc.Digest, got)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (s *Store) writeImageMeta(ref string, m *oci.Manifest, cfg *oci.ImageConfig) error {
	_, repo, tag := parseRef(ref)
	dir := filepath.Join(s.root, "images", sanitisePath(repo), sanitisePath(tag))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	mData, _ := json.MarshalIndent(m, "", "  ")
	if err := atomicWrite(filepath.Join(dir, "manifest.json"), mData); err != nil {
		return err
	}
	cData, _ := json.MarshalIndent(cfg, "", "  ")
	return atomicWrite(filepath.Join(dir, "config.json"), cData)
}

// ─── Registry HTTP client ─────────────────────────────────────────────────────

type registryClient struct {
	registry string
	repo     string
	token    string
	hc       *http.Client
}

func (c *registryClient) auth(ctx context.Context) error {
	url := fmt.Sprintf("%s?service=registry.docker.io&scope=repository:%s:pull", tokenEndpoint, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: %s", resp.Status)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return err
	}
	c.token = tok.Token
	return nil
}

func (c *registryClient) manifest(ctx context.Context, ref string) (*oci.Manifest, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.registry, c.repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", strings.Join([]string{
		oci.MediaTypeImageManifest,
		oci.MediaTypeImageIndex,
		oci.MediaTypeDockerManifestV2,
		oci.MediaTypeDockerManifestList,
	}, ", "))

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest %s: %s", ref, resp.Status)
	}

	ct := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	if strings.Contains(ct, "index") || strings.Contains(ct, "manifest.list") {
		var idx oci.Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, err
		}
		for _, m := range idx.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				return c.manifest(ctx, m.Digest)
			}
		}
		return nil, errors.New("no linux/amd64 manifest in index")
	}

	var m oci.Manifest
	return &m, json.Unmarshal(body, &m)
}

func (c *registryClient) blob(ctx context.Context, digest string) (io.ReadCloser, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", c.registry, c.repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("blob %s: %s", digest, resp.Status)
	}
	return resp.Body, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func parseRef(ref string) (registry, repo, tag string) {
	registry = defaultRegistry
	tag = "latest"

	if idx := strings.Index(ref, "@"); idx != -1 {
		tag = ref[idx+1:]
		ref = ref[:idx]
	} else if idx := strings.LastIndex(ref, ":"); idx != -1 && !strings.Contains(ref[idx:], "/") {
		tag = ref[idx+1:]
		ref = ref[:idx]
	}

	parts := strings.SplitN(ref, "/", 2)
	switch {
	case len(parts) == 2 && strings.ContainsAny(parts[0], ".:"):
		registry = parts[0]
		repo = parts[1]
	case len(parts) == 1:
		repo = "library/" + parts[0]
	default:
		repo = ref
	}
	return
}

func sanitisePath(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(s)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
