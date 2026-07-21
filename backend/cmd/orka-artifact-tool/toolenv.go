package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	maxResolverBackends     = 32
	maxResolverBuilds       = 128
	maxResolverCacheEntries = 64
	maxResolverCacheBytes   = 1 << 20
)

// buildResolver serves authenticated, header-scoped artifact requests across consumers.
type buildResolver struct {
	defaultCfg    storage.Config
	defaultBucket string
	defaultPrefix string
	modelBudget   int
	gcsBudget     int

	mu          sync.Mutex
	clock       uint64
	backends    map[string]*bucketBackend
	backendUsed map[string]uint64
	builds      map[string]*buildEntry
}

type bucketBackend struct {
	key     string
	bucket  string
	backend storage.Backend
}

type buildEntry struct {
	browser artifacts.Browser
	cache   *tools.Cache
	used    uint64
}

func newBuildResolver(defaultCfg storage.Config, defaultBucket, defaultPrefix string) (*buildResolver, error) {
	r := &buildResolver{
		defaultCfg:    defaultCfg,
		defaultBucket: defaultBucket,
		defaultPrefix: normalizeBuildPrefix(defaultPrefix),
		modelBudget:   envPositiveInt("MODEL_BYTE_BUDGET", defaultScopeModelBytes),
		gcsBudget:     envPositiveInt("GCS_BYTE_BUDGET", defaultScopeGCSBytes),
		backends:      map[string]*bucketBackend{},
		backendUsed:   map[string]uint64{},
		builds:        map[string]*buildEntry{},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.backendLocked(defaultBucket, storage.Config{}); err != nil {
		return nil, err
	}
	return r, nil
}

func envPositiveInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func (r *buildResolver) effectiveCfg(bucket string, override storage.Config) storage.Config {
	cfg := r.defaultCfg
	if bucket == "" {
		bucket = r.defaultBucket
	}
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

func storageRouteKey(cfg storage.Config) string {
	return strings.Join([]string{string(cfg.Provider), cfg.Bucket, cfg.Base, cfg.WebBase, cfg.ProwBase}, "\x00")
}

func (r *buildResolver) backendLocked(bucket string, override storage.Config) (*bucketBackend, error) {
	cfg := r.effectiveCfg(bucket, override)
	key := storageRouteKey(cfg)
	if bb, ok := r.backends[key]; ok {
		r.touchBackendLocked(key)
		return bb, nil
	}
	backend, err := storage.New(cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("storage route provider=%s bucket=%s: %w", cfg.Provider, cfg.Bucket, err)
	}
	bb := &bucketBackend{key: key, bucket: cfg.Bucket, backend: backend}
	r.backends[key] = bb
	r.touchBackendLocked(key)
	r.evictBackendsLocked(key)
	return bb, nil
}

func (r *buildResolver) touchBackendLocked(key string) {
	r.clock++
	r.backendUsed[key] = r.clock
}

func (r *buildResolver) evictBackendsLocked(keep string) {
	for len(r.backends) > maxResolverBackends {
		oldest := oldestKey(r.backendUsed, keep)
		if oldest == "" {
			return
		}
		delete(r.backends, oldest)
		delete(r.backendUsed, oldest)
	}
}

func oldestKey(used map[string]uint64, keep string) string {
	var oldest string
	var tick uint64
	for key, value := range used {
		if key == keep || oldest != "" && value >= tick {
			continue
		}
		oldest, tick = key, value
	}
	return oldest
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

func (r *buildResolver) resolve(bucket, prefix, scope string, override storage.Config) (*bucketBackend, *buildEntry, string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bb, err := r.backendLocked(bucket, override)
	if err != nil {
		return nil, nil, "", "", err
	}
	prefix = normalizeBuildPrefix(prefix)
	if prefix == "" {
		prefix = r.defaultPrefix
	}
	if scope == "" {
		scope = prefix
	}
	key := bb.key + "\x00" + scope + "\x00" + prefix
	entry, ok := r.builds[key]
	if !ok {
		entry = &buildEntry{
			browser: artifacts.NewUncachedBackendBrowser(bb.backend, bb.bucket, prefix, prefix),
			cache:   tools.NewBoundedCache(maxResolverCacheEntries, maxResolverCacheBytes),
		}
		r.builds[key] = entry
	}
	r.clock++
	entry.used = r.clock
	r.evictBuildsLocked(key)
	return bb, entry, bb.backend.WebURL(prefix), prefix, nil
}

func (r *buildResolver) evictBuildsLocked(keep string) {
	for len(r.builds) > maxResolverBuilds {
		var oldest string
		var tick uint64
		for key, entry := range r.builds {
			if key == keep || oldest != "" && entry.used >= tick {
				continue
			}
			oldest, tick = key, entry.used
		}
		if oldest == "" {
			return
		}
		delete(r.builds, oldest)
	}
}

func (r *buildResolver) aiEnv(bucket, prefix, scope string, override storage.Config) (*tools.Env, *scopeBudget, error) {
	_, entry, web, _, err := r.resolve(bucket, prefix, scope, override)
	if err != nil {
		return nil, nil, err
	}
	budget := newScopeBudget(r.modelBudget, r.gcsBudget)
	modelRemaining, gcsRemaining := budget.remaining()
	return &tools.Env{
		Browser:             &budgetBrowser{Browser: entry.browser, budget: budget},
		Cache:               entry.cache,
		WebURLBase:          web,
		RemainingModelBytes: modelRemaining,
		RemainingGCSBytes:   gcsRemaining,
	}, budget, nil
}

func (r *buildResolver) toolEnvFor(bucket, prefix, scope string, override storage.Config) (*toolEnv, *scopeBudget, error) {
	bb, entry, web, p, err := r.resolve(bucket, prefix, scope, override)
	if err != nil {
		return nil, nil, err
	}
	budget := newScopeBudget(r.modelBudget, r.gcsBudget)
	wrap := func(browser artifacts.Browser) artifacts.Browser {
		return &budgetBrowser{Browser: browser, budget: budget}
	}
	return &toolEnv{
		backend:     &budgetBackend{Backend: bb.backend, budget: budget},
		rawBackend:  bb.backend,
		bucket:      bb.bucket,
		buildPrefix: p,
		webURLBase:  web,
		browser:     wrap(entry.browser),
		browserForBuild: func(prefix, display string) artifacts.Browser {
			return wrap(artifacts.NewUncachedBackendBrowser(bb.backend, bb.bucket, prefix, display))
		},
	}, budget, nil
}

func requestStorage(r *http.Request) storage.Config {
	return storage.Config{
		Provider: storage.Provider(strings.TrimSpace(r.Header.Get("X-Storage-Provider"))),
		Base:     strings.TrimSpace(r.Header.Get("X-Storage-Base")),
		WebBase:  strings.TrimSpace(r.Header.Get("X-Web-Base")),
		ProwBase: strings.TrimSpace(r.Header.Get("X-Prow-Base")),
	}
}

func requestBucket(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Bucket"))
}

func requestBuild(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Build-Prefix"))
}

func requestScope(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(orka.ToolScopeHeader))
}

// toolEnv is the per-request context handed to every self-registered quality
// tool. It exposes the build-scoped Browser plus the raw backend and a factory
// so cross-build tools (recurrence, diff_last_passing) can reach other builds.
type toolEnv struct {
	backend     storage.Backend
	rawBackend  storage.Backend
	bucket      string
	buildPrefix string
	webURLBase  string
	browser     artifacts.Browser
	evidence    *evidenceAttestor
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

func writeToolError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, map[string]any{"error": message})
}

// requirePOST writes 405 and returns false if the method is not POST.
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return false
	}
	return true
}
