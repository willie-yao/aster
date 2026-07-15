package main

import (
	"net/http/httptest"
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
