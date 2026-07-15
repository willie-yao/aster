// Command orka-artifact-tool is an HTTP shim that exposes the engine's artifact
// tools (filesystem: list/read/tail/grep/find and k8s:
// discover_clusters/find_my_cluster/... over a Prow build's GCS artifact tree)
// as plain HTTP endpoints so Orka Tool CRDs can call them. The engine's domain
// code is reused unchanged; only the transport is HTTP.
//
// One shim serves any build from any bucket: a request selects its bucket via
// the X-Bucket header (or "bucket" body field) and its build via X-Build-Prefix
// (or "build"), so a single service backs many concurrent per-build Tasks across
// consumers (e.g. capz's kubernetes-ci-logs and istio's istio-prow).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	bucket := env("BUCKET", "kubernetes-ci-logs")
	buildPrefix := env("BUILD_PREFIX", "logs/periodic-cluster-api-provider-azure-e2e-main/2073230525962653696/")
	addr := env("ADDR", ":8080")

	// Default storage config. STORAGE_PROVIDER selects the backend (gcs, gcsweb
	// over an S3 gateway, local); the *_BASE vars point gcsweb at its gateway.
	// Per-request X-Storage-* headers override these, so one shim can serve
	// consumers on different providers.
	defaultCfg := storage.Config{
		Provider: storage.Provider(env("STORAGE_PROVIDER", string(storage.ProviderGCS))),
		Base:     os.Getenv("STORAGE_BASE"),
		WebBase:  os.Getenv("STORAGE_WEB_BASE"),
		ProwBase: os.Getenv("STORAGE_PROW_BASE"),
	}

	reg := tools.NewRegistry()
	filesystem.Register(reg)
	k8s.Register(reg)

	// The resolver serves any build from any bucket per-request (via the
	// X-Bucket / X-Build-Prefix headers or "bucket" / "build" body fields), so
	// one shared service backs many concurrent per-build analysis Tasks across
	// consumers. Absent a selector it uses BUCKET / BUILD_PREFIX / STORAGE_*.
	resolver, err := newBuildResolver(defaultCfg, bucket, buildPrefix)
	if err != nil {
		log.Fatalf("newBuildResolver: %v", err)
	}

	// Self-registered deterministic quality tools each live in their own file
	// and register via registerQTool (see validate.go for the template). Each
	// request resolves its own bucket- and build-scoped toolEnv.
	for _, qt := range qtools {
		qt := qt
		http.HandleFunc(qt.route, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			tenv := resolver.toolEnvFor(requestBucket(r, body), requestBuild(r, body), requestStorage(r))
			r.Body = io.NopCloser(bytes.NewReader(body))
			qt.h(tenv, w, r)
		})
	}

	// POST /tool/{name}; body is the raw JSON tool arguments. Response is the
	// tool's Payload as JSON, exactly what the engine would hand the model.
	http.HandleFunc("/tool/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Path[len("/tool/"):]
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		// Engine tools ignore the extra "bucket"/"build" fields; the resolver
		// uses them to pick the bucket- and build-scoped Env.
		res := reg.Dispatch(ctx, resolver.aiEnv(requestBucket(r, raw), requestBuild(r, raw), requestStorage(r)), name, json.RawMessage(raw))
		log.Printf("🛠  %s args=%s bytes=%d", name, truncate(raw, 200), res.BytesFetched)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res.Payload)
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("orka-artifact-tool on %s default-bucket=%s build=%s", addr, bucket, buildPrefix)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
