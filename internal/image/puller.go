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
	"net/http"
	"os"
	"path/filepath"
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
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	reg, repo, tag := parseRef(ref)
	client := &registryClient{
		registry: reg,
		repo:     repo,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}

	if err := client.auth(ctx); err != nil {
		return nil, fmt.Errorf("pull auth %s: %w", ref, err)
	}

	manifest, err := client.manifest(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("pull manifest %s: %w", ref, err)
	}

	cfgBlob, err := s.fetchBlob(ctx, client, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("pull config: %w", err)
	}
	var imgCfg oci.ImageConfig
	if err := json.Unmarshal(cfgBlob, &imgCfg); err != nil {
		return nil, fmt.Errorf("decode image config: %w", err)
	}

	if err := s.pullLayers(ctx, client, manifest.Layers); err != nil {
		return nil, fmt.Errorf("pull layers: %w", err)
	}

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

// pullLayers fetches all layers with bounded concurrency.
func (s *Store) pullLayers(ctx context.Context, client *registryClient, layers []oci.Descriptor) error {
	type result struct{ err error }
	results := make(chan result, len(layers))
	sem := make(chan struct{}, maxLayerConcurrent)

	var wg sync.WaitGroup
	for _, layer := range layers {
		if s.HasBlob(layer.Digest) {
			continue
		}
		wg.Add(1)
		go func(desc oci.Descriptor) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, err := s.fetchBlob(ctx, client, desc)
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

// fetchBlob downloads a blob, verifies digest, and writes it atomically.
func (s *Store) fetchBlob(ctx context.Context, client *registryClient, desc oci.Descriptor) ([]byte, error) {
	dest := s.BlobPath(desc.Digest)

	if data, err := os.ReadFile(dest); err == nil {
		return data, nil
	}

	rc, err := client.blob(ctx, desc.Digest)
	if err != nil {
		return nil, fmt.Errorf("fetch blob %s: %w", desc.Digest, err)
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-blob-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	h := sha256.New()
	tee := io.TeeReader(rc, h)
	buf := make([]byte, layerBufSize)

	if _, err := io.CopyBuffer(tmp, tee, buf); err != nil {
		cleanup()
		return nil, fmt.Errorf("stream blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	tmp.Close()

	got := fmt.Sprintf("sha256:%x", h.Sum(nil))
	if got != desc.Digest {
		os.Remove(tmpName)
		return nil, fmt.Errorf("digest mismatch: want %s got %s", desc.Digest, got)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return nil, err
	}

	return os.ReadFile(dest)
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
