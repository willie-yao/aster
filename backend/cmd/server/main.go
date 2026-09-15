// Command server serves the dashboard's pre-computed JSON over HTTP for the
// Kubernetes-native deploy mode. It serves the same /data/*.json contract the
// static Pages site reads, plus /api/capabilities so the frontend can light up
// server-only features. The static Pages mode keeps working unchanged.
//
// Admin-gated interactive features are enabled when -project-dir is set and
// AUTH_MODE selects an auth mechanism. ANALYSIS_CHAT_ENABLED enables read-only
// chat. ACTIONS_ENABLED controls GitHub writes and defaults off when any
// read-only interactive feature is enabled, otherwise on. BOT_TOKEN is required
// only when write actions are enabled.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/willie-yao/aster/backend/internal/credentialenv"
	"github.com/willie-yao/aster/backend/internal/server"
)

var (
	version  = "dev"
	commit   = "dev"
	imageTag = "dev"
)

func main() {
	credentialenv.SanitizeAndReport()
	var (
		addr       string
		dataDir    string
		staticDir  string
		projectDir string
		mock       bool
	)
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&dataDir, "data-dir", "data", "directory of fetcher JSON output served at /data")
	flag.StringVar(&staticDir, "static-dir", "", "optional built frontend (dist) served at / with SPA fallback")
	flag.StringVar(&projectDir, "project-dir", "", "project.yaml directory; enables admin features when set with AUTH_MODE")
	flag.BoolVar(&mock, "mock", false, "serve every admin feature from in-memory fakes for local frontend development; never use outside a developer machine")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hstsEnabled, err := optionalBoolEnv("HSTS_ENABLED", false)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	opts := server.Options{
		DataDir:      dataDir,
		StaticDir:    staticDir,
		Capabilities: server.DefaultCapabilities(),
		HSTSEnabled:  hstsEnabled,
	}
	opts.Capabilities.Engine = server.EngineInfo{Version: version, Commit: commit, ImageTag: imageTag}

	// Enable admin-gated features only when a project config and an auth mode are
	// both provided. Otherwise the server stays read-only.
	switch {
	case mock:
		if err := enableMockFeatures(&opts, projectDir, dataDir); err != nil {
			log.Fatalf("server: enabling mock features: %v", err)
		}
	case projectDir != "" && os.Getenv("AUTH_MODE") != "":
		if err := enableInteractiveFeatures(ctx, &opts, projectDir, dataDir); err != nil {
			log.Fatalf("server: enabling interactive features: %v", err)
		}
		log.Printf("🔐 admin features enabled (auth mode: %s)", opts.AuthMode)
	default:
		log.Println("interactive features disabled (set -project-dir and AUTH_MODE to enable)")
	}

	handler, err := server.Handler(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// Bound the header read so a slow-header client cannot tie up a
		// connection. WriteTimeout is intentionally unset: an action request
		// (draft a fix PR) can legitimately run for minutes. IdleTimeout caps
		// idle keep-alive connections.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("🌐 serving %s -> data=%s static=%q", addr, dataDir, staticDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: graceful shutdown: %v", err)
	}
	if waiter, ok := opts.AnalysisChat.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for analysis chat turns: %v", err)
		}
	}
	if waiter, ok := opts.PullRequestEscalation.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for pull request escalations: %v", err)
		}
	}
	if waiter, ok := opts.SharedFailureEscalation.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for shared failure escalations: %v", err)
		}
	}
	if waiter, ok := opts.Actions.(interface{ Wait(context.Context) error }); ok {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer waitCancel()
		if err := waiter.Wait(waitCtx); err != nil {
			log.Printf("server: waiting for action requests: %v", err)
		}
	}
}
