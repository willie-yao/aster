package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/devmock"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/server"
)

// defaultMockProjectDir is the docs-only example project the mock server falls
// back to. Only its deterministic resolution behavior is used, so no consumer
// repository has to be checked out to develop against the mock.
const defaultMockProjectDir = "configs/example"

// defaultMockLogin is the admin identity a mock sign-in establishes.
const defaultMockLogin = "mock-admin"

// enableMockFeatures wires the in-memory stand-ins so every feature the
// deployed dashboard advertises is reachable without AI credentials, GitHub
// write access, or a Kubernetes cluster. The routes, auth middleware, CSRF
// guard, and wire formats are the production ones; only the work behind them
// is fabricated.
func enableMockFeatures(opts *server.Options, projectDir, dataDir string) error {
	if strings.TrimSpace(projectDir) == "" {
		projectDir = defaultMockProjectDir
	}
	cfg, err := project.Load(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	latency, err := mockLatency()
	if err != nil {
		return err
	}
	if err := devmock.Seed(dataDir); err != nil {
		return err
	}
	services, err := devmock.New(cfg, devmock.Options{DataDir: dataDir, Latency: latency})
	if err != nil {
		return err
	}
	login := os.Getenv("MOCK_LOGIN")
	if strings.TrimSpace(login) == "" {
		login = defaultMockLogin
	}
	authenticator, err := auth.NewMockAuthenticator(login)
	if err != nil {
		return err
	}
	pricing, err := aiusage.NewPriceTable(devmock.Pricing)
	if err != nil {
		return fmt.Errorf("configuring mock usage pricing: %w", err)
	}

	opts.Auth = authenticator
	// The deployed dashboard signs admins in with OAuth. Reporting the same mode
	// is what puts the sign-in and sign-out controls on screen, which is half of
	// what the authenticated UI does.
	opts.AuthMode = "oauth"
	opts.LoginURL = "/api/auth/login"
	opts.Actions = services.Actions
	opts.AnalysisChat = services.AnalysisChat
	opts.ChatFix = services.ChatFix
	opts.PullRequestEscalation = services.PullRequestEscalation
	opts.SharedFailureEscalation = services.SharedFailureEscalation
	opts.AIUsageEnabled = true
	opts.AIUsageModel = "mock-model"
	opts.AIUsagePricing = pricing
	opts.AIUsagePricingRule = devmock.PricingRule
	opts.Capabilities.Features.JUnitChatFix = true
	opts.Capabilities.Features.ChatFixMinConfidence = cfg.EffectiveFixPRs().MinConfidence

	log.Printf("🎭 mock mode: every admin feature is fabricated, every request signs in as %q, and nothing reaches GitHub or a model provider", login)
	log.Printf("🎭 mock mode grants admin to anyone who can reach this server; bind it to localhost only")
	return nil
}

// mockLatency reads how long a fabricated model call should take, so pending
// and streaming states can be slowed down or removed while iterating.
func mockLatency() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv("MOCK_LATENCY"))
	if value == "" {
		return 0, nil
	}
	latency, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid MOCK_LATENCY %q: %w", value, err)
	}
	if latency < 0 {
		return 0, fmt.Errorf("invalid MOCK_LATENCY %q: must not be negative", value)
	}
	return latency, nil
}
