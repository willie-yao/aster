package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func TestEffectiveCfg_DefaultsToGCS(t *testing.T) {
	r := &buildResolver{defaultCfg: storage.Config{}}
	got := r.effectiveCfg("my-bucket", storage.Config{})
	if got.Provider != storage.ProviderGCS {
		t.Errorf("Provider = %q, want gcs (empty default must fall back to gcs)", got.Provider)
	}
	if got.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q, want my-bucket", got.Bucket)
	}
}

func TestEffectiveCfg_DefaultProviderHonored(t *testing.T) {
	r := &buildResolver{defaultCfg: storage.Config{
		Provider: storage.ProviderGCSWeb,
		Base:     "https://gcsweb.example.io/s3",
		ProwBase: "https://prow.example.io/view/s3",
	}}
	got := r.effectiveCfg("istio-prow", storage.Config{})
	if got.Provider != storage.ProviderGCSWeb {
		t.Errorf("Provider = %q, want gcsweb (env default)", got.Provider)
	}
	if got.Base != "https://gcsweb.example.io/s3" || got.ProwBase != "https://prow.example.io/view/s3" {
		t.Errorf("Base/ProwBase not carried from default: %+v", got)
	}
	if got.Bucket != "istio-prow" {
		t.Errorf("Bucket = %q, want istio-prow", got.Bucket)
	}
}

func TestEffectiveCfg_RequestOverridesDefault(t *testing.T) {
	// A shared shim defaulting to gcs must still serve a gcsweb consumer when the
	// per-request override says so.
	r := &buildResolver{defaultCfg: storage.Config{Provider: storage.ProviderGCS}}
	override := storage.Config{Provider: storage.ProviderGCSWeb, Base: "https://gcsweb.example.io/s3"}
	got := r.effectiveCfg("some-bucket", override)
	if got.Provider != storage.ProviderGCSWeb {
		t.Errorf("Provider = %q, want gcsweb (override wins)", got.Provider)
	}
	if got.Base != "https://gcsweb.example.io/s3" {
		t.Errorf("Base = %q, want the overridden gateway", got.Base)
	}
}

func TestRequestStorage_ParsesHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/tool/list_artifacts", nil)
	req.Header.Set("X-Storage-Provider", "gcsweb")
	req.Header.Set("X-Storage-Base", "https://gcsweb.example.io/s3")
	req.Header.Set("X-Prow-Base", "https://prow.example.io/view/s3")
	got := requestStorage(req)
	if got.Provider != storage.ProviderGCSWeb {
		t.Errorf("Provider = %q, want gcsweb", got.Provider)
	}
	if got.Base != "https://gcsweb.example.io/s3" {
		t.Errorf("Base = %q", got.Base)
	}
	if got.ProwBase != "https://prow.example.io/view/s3" {
		t.Errorf("ProwBase = %q", got.ProwBase)
	}
}

func TestRequestStorage_EmptyWhenNoHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/tool/list_artifacts", nil)
	got := requestStorage(req)
	if got.Provider != "" || got.Base != "" || got.WebBase != "" || got.ProwBase != "" {
		t.Errorf("expected empty override with no headers, got %+v", got)
	}
}

func TestRequestSelectorsUseHeadersOnly(t *testing.T) {
	req := httptest.NewRequest("POST", "/tool/read_artifact", strings.NewReader(`{"bucket":"other","build":"logs/other/1/"}`))
	if got := requestBucket(req); got != "" {
		t.Fatalf("bucket = %q, want header-only empty value", got)
	}
	if got := requestBuild(req); got != "" {
		t.Fatalf("build = %q, want header-only empty value", got)
	}
	req.Header.Set("X-Bucket", "trusted")
	req.Header.Set("X-Build-Prefix", "logs/trusted/1/")
	if requestBucket(req) != "trusted" || requestBuild(req) != "logs/trusted/1/" {
		t.Fatalf("header selectors were not honored")
	}
}

func TestResolverFailsClosedForInvalidStorageRoute(t *testing.T) {
	resolver, err := newBuildResolver(storage.Config{Provider: storage.ProviderLocal, Base: t.TempDir()}, "", "logs/job/1/")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.aiEnv("bucket", "logs/job/1/", "scope", storage.Config{Provider: "invalid"}); err == nil {
		t.Fatal("invalid explicit storage provider fell back instead of failing")
	}
}

func TestResolverBoundsBuildScopes(t *testing.T) {
	resolver, err := newBuildResolver(storage.Config{Provider: storage.ProviderLocal, Base: t.TempDir()}, "", "logs/job/1/")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxResolverBuilds+20; i++ {
		prefix := fmt.Sprintf("logs/job/%d/", i)
		if _, _, err := resolver.aiEnv("", prefix, fmt.Sprintf("scope-%d", i), storage.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if len(resolver.builds) != maxResolverBuilds {
		t.Fatalf("build scopes = %d, want %d", len(resolver.builds), maxResolverBuilds)
	}
}

func TestResolverDoesNotRetainCrossBuildBrowsers(t *testing.T) {
	resolver, err := newBuildResolver(storage.Config{Provider: storage.ProviderLocal, Base: t.TempDir()}, "", "logs/job/1/")
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := resolver.toolEnvFor("", "logs/job/1/", "scope", storage.Config{})
	if err != nil {
		t.Fatal(err)
	}
	first := env.browserForBuild("logs/job/2/", "job/2")
	second := env.browserForBuild("logs/job/2/", "job/2")
	if first.(*budgetBrowser).Browser == second.(*budgetBrowser).Browser {
		t.Fatal("cross-build browser was retained across lookups")
	}
}

func TestResolverBoundsToolCache(t *testing.T) {
	resolver, err := newBuildResolver(storage.Config{Provider: storage.ProviderLocal, Base: t.TempDir()}, "", "logs/job/1/")
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := resolver.aiEnv("", "logs/job/1/", "scope", storage.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxResolverCacheEntries+20; i++ {
		env.Cache.Set(fmt.Sprintf("key-%d", i), strings.Repeat("x", 1024))
	}
	if env.Cache.Len() > maxResolverCacheEntries {
		t.Fatalf("cache entries = %d, want <= %d", env.Cache.Len(), maxResolverCacheEntries)
	}
	if env.Cache.Bytes() > maxResolverCacheBytes {
		t.Fatalf("cache bytes = %d, want <= %d", env.Cache.Bytes(), maxResolverCacheBytes)
	}
}
