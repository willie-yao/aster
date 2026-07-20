// Command orka-artifact-tool exposes the engine's read-only artifact tools as
// authenticated HTTP endpoints for Orka Tool CRDs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	authToken, err := loadAuthToken()
	if err != nil {
		log.Fatal(err)
	}
	evidence := newEvidenceAttestor(authToken)

	defaultCfg := storage.Config{
		Provider: storage.Provider(env("STORAGE_PROVIDER", string(storage.ProviderGCS))),
		Base:     os.Getenv("STORAGE_BASE"),
		WebBase:  os.Getenv("STORAGE_WEB_BASE"),
		ProwBase: os.Getenv("STORAGE_PROW_BASE"),
	}

	reg := tools.NewRegistry()
	filesystem.Register(reg)
	k8s.Register(reg)
	resolver, err := newBuildResolver(defaultCfg, bucket, buildPrefix)
	if err != nil {
		log.Fatalf("newBuildResolver: %v", err)
	}

	mux := http.NewServeMux()
	for _, qt := range qtools {
		qt := qt
		mux.HandleFunc(qt.route, requireBearer(authToken, func(w http.ResponseWriter, r *http.Request) {
			body, err := readToolBody(w, r)
			if err != nil {
				return
			}
			tenv, budget, err := resolver.toolEnvFor(requestBucket(r), requestBuild(r), requestScope(r), requestStorage(r))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			tenv.evidence = evidence
			r.Body = io.NopCloser(bytes.NewReader(body))
			writeBudgetedResponse(w, budget, func(buffer http.ResponseWriter) {
				qt.h(tenv, buffer, r)
			})
		}))
	}

	mux.HandleFunc("/tool/", requireBearer(authToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Path[len("/tool/"):]
		raw, err := readToolBody(w, r)
		if err != nil {
			return
		}
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		env, budget, err := resolver.aiEnv(requestBucket(r), requestBuild(r), requestScope(r), requestStorage(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res := reg.Dispatch(ctx, env, name, json.RawMessage(raw))
		attachEvidenceToken(evidence, requestScope(r), name, res.BytesFetched, res.Payload)
		log.Printf("🛠 %s args=%s bytes=%d", name, truncate(raw, 200), res.BytesFetched)
		writeBudgetedResponse(w, budget, func(buffer http.ResponseWriter) {
			buffer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(buffer).Encode(res.Payload)
		})
	}))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("orka-artifact-tool on %s default-bucket=%s build=%s", addr, bucket, buildPrefix)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      70 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	log.Fatal(server.ListenAndServe())
}

func readToolBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read request body", http.StatusBadRequest)
		}
		return nil, err
	}
	return body, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
