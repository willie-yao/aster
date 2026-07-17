package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// buildResolver serves any build from any bucket in one process so a single
// shared tool service can back many concurrent per-build analysis Tasks across
// consumers. A request selects its bucket via the X-Bucket header or a "bucket"
// body field, and its build via X-Build-Prefix or a "build" body field; absent
// those, defaultBucket / defaultPrefix are used. Backends, Browsers and caches
// are memoized per bucket and per (bucket, build).
type buildResolver struct {
	defaultCfg    storage.Config
	defaultBucket string
	defaultPrefix string

	mu       sync.Mutex
	buckets  map[string]*bucketBackend    // bucket -> backend + factory
	browsers map[string]artifacts.Browser // "bucket\x00prefix" -> browser
	caches   map[string]*tools.Cache      // "bucket\x00prefix" -> cache
}

// bucketBackend is the storage backend + artifact factory for one bucket.
type bucketBackend struct {
	bucket  string
	backend storage.Backend
	factory *artifacts.BackendFactory
}

func newBuildResolver(defaultCfg storage.Config, defaultBucket, defaultPrefix string) (*buildResolver, error) {
	r := &buildResolver{
		defaultCfg:    defaultCfg,
		defaultBucket: defaultBucket,
		defaultPrefix: normalizeBuildPrefix(defaultPrefix),
		buckets:       map[string]*bucketBackend{},
		browsers:      map[string]artifacts.Browser{},
		caches:        map[string]*tools.Cache{},
	}
	// Eagerly create the default bucket so a misconfiguration fails fast and the
	// per-request fallback always has a backend to fall back to.
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.bucketLocked(defaultBucket, storage.Config{}); r.buckets[defaultBucket] == nil {
		return nil, err
	}
	return r, nil
}

// effectiveCfg merges the shim's default storage config with a per-request
// override (X-Storage-* headers), so a single shim can serve consumers on
// different storage providers (gcs, gcsweb over an S3 gateway, ...).
func (r *buildResolver) effectiveCfg(bucket string, override storage.Config) storage.Config {
	cfg := r.defaultCfg
	cfg.Bucket = bucket
	if override.Provider != "" {
		cfg.Provider = override.Provider
	}
	if override.Base != "" {
		cfg.Base = override.Base
	}
	if override.WebBase != "" {
		cfg.WebBase = override.WebBase
	}
	if override.ProwBase != "" {
		cfg.ProwBase = override.ProwBase
	}
	if cfg.Provider == "" {
		cfg.Provider = storage.ProviderGCS
	}
	return cfg
}

// bucketLocked returns the backend for bucket, creating it on first use from the
// merged default + override storage config. The caller must hold r.mu. On a bad
// bucket it logs and falls back to the default.
func (r *buildResolver) bucketLocked(bucket string, override storage.Config) (*bucketBackend, error) {
	if bucket == "" {
		bucket = r.defaultBucket
	}
	if bb, ok := r.buckets[bucket]; ok {
		return bb, nil
	}
	backend, err := storage.New(r.effectiveCfg(bucket, override), nil)
	if err != nil {
		if bb, ok := r.buckets[r.defaultBucket]; ok {
			log.Printf("⚠ bucket %q init failed (%v); using default %q", bucket, err, r.defaultBucket)
			return bb, nil
		}
		return nil, fmt.Errorf("storage.New bucket=%s: %w", bucket, err)
	}
	bb := &bucketBackend{bucket: bucket, backend: backend, factory: artifacts.NewBackendFactory(backend, bucket)}
	r.buckets[bucket] = bb
	return bb, nil
}

func normalizeBuildPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// resolve returns the backend, Browser, web-URL base and normalized prefix for a
// build in a bucket, memoizing the Browser per (bucket, build).
func (r *buildResolver) resolve(bucket, prefix string, override storage.Config) (*bucketBackend, artifacts.Browser, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bb, _ := r.bucketLocked(bucket, override)
	prefix = normalizeBuildPrefix(prefix)
	if prefix == "" {
		prefix = r.defaultPrefix
	}
	key := bb.bucket + "\x00" + prefix
	b, ok := r.browsers[key]
	if !ok {
		b = bb.factory.ForBuild(prefix, prefix)
		r.browsers[key] = b
	}
	return bb, b, bb.backend.WebURL(prefix), prefix
}

func (r *buildResolver) cache(bucket, prefix string) *tools.Cache {
	prefix = normalizeBuildPrefix(prefix)
	if prefix == "" {
		prefix = r.defaultPrefix
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bb, _ := r.bucketLocked(bucket, storage.Config{})
	key := bb.bucket + "\x00" + prefix
	c, ok := r.caches[key]
	if !ok {
		c = tools.NewCache()
		r.caches[key] = c
	}
	return c
}

// aiEnv builds the engine-tool Env for a build in a bucket.
func (r *buildResolver) aiEnv(bucket, prefix string, override storage.Config) *tools.Env {
	bb, b, web, p := r.resolve(bucket, prefix, override)
	return &tools.Env{
		Browser:             b,
		Cache:               r.cache(bb.bucket, p),
		WebURLBase:          web,
		RemainingModelBytes: 1 << 30,
		RemainingGCSBytes:   1 << 30,
	}
}

// toolEnvFor builds the quality-tool toolEnv for a build in a bucket.
func (r *buildResolver) toolEnvFor(bucket, prefix string, override storage.Config) *toolEnv {
	bb, b, web, p := r.resolve(bucket, prefix, override)
	return &toolEnv{
		backend:         bb.backend,
		bucket:          bb.bucket,
		buildPrefix:     p,
		webURLBase:      web,
		browser:         b,
		browserForBuild: bb.factory.ForBuild,
	}
}

// requestStorage extracts per-request storage overrides from the X-Storage-*
// headers, so a shared shim can serve consumers on different providers. Empty
// fields fall back to the shim's default storage config.
func requestStorage(r *http.Request) storage.Config {
	return storage.Config{
		Provider: storage.Provider(strings.TrimSpace(r.Header.Get("X-Storage-Provider"))),
		Base:     strings.TrimSpace(r.Header.Get("X-Storage-Base")),
		WebBase:  strings.TrimSpace(r.Header.Get("X-Web-Base")),
		ProwBase: strings.TrimSpace(r.Header.Get("X-Prow-Base")),
	}
}

// requestBucket extracts the caller-selected GCS bucket from the X-Bucket header
// (preferred) or a "bucket" field in the JSON body. Empty means default.
func requestBucket(r *http.Request, body []byte) string {
	if h := strings.TrimSpace(r.Header.Get("X-Bucket")); h != "" {
		return h
	}
	var meta struct {
		Bucket string `json:"bucket"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &meta)
	}
	return meta.Bucket
}

// requestBuild extracts the caller-selected build prefix from the X-Build-Prefix
// header (preferred) or a "build" field in the JSON body. Empty means default.
func requestBuild(r *http.Request, body []byte) string {
	if h := strings.TrimSpace(r.Header.Get("X-Build-Prefix")); h != "" {
		return h
	}
	var meta struct {
		Build string `json:"build"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &meta)
	}
	return meta.Build
}

// toolEnv is the per-request context handed to every self-registered quality
// tool. It exposes the build-scoped Browser plus the raw backend and a factory
// so cross-build tools (recurrence, diff_last_passing) can reach other builds.
type toolEnv struct {
	backend     storage.Backend
	bucket      string
	buildPrefix string
	webURLBase  string
	browser     artifacts.Browser
	// browserForBuild returns a Browser bound to another build in the same
	// bucket. prefix is the bucket-relative, trailing-slashed build directory.
	browserForBuild func(prefix, display string) artifacts.Browser
}

// qtool is one self-registered HTTP quality tool: an exact route plus its
// handler. Tools register themselves from an init() so adding a tool never
// requires editing main.go.
type qtool struct {
	route string
	h     func(*toolEnv, http.ResponseWriter, *http.Request)
}

var qtools []qtool

// registerQTool records a quality tool. Call it from a file-level init() in each
// tool's own file (see validate.go for the reference template).
func registerQTool(route string, h func(*toolEnv, http.ResponseWriter, *http.Request)) {
	qtools = append(qtools, qtool{route: route, h: h})
}

// requestCtx returns a per-request context with a 60s cap for tool work.
func requestCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 60*time.Second)
}

// readArgs reads and JSON-unmarshals the request body (capped at 1 MiB) into v.
// An empty body is treated as "{}". A malformed body returns an error the caller
// should surface as 400.
func readArgs(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// writeJSON writes v as a successful JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus writes v as a JSON response with status.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requirePOST writes 405 and returns false if the method is not POST.
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return false
	}
	return true
}
