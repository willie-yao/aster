package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// FinalizedSideEffectsOptions identifies finalized dashboard data and its project config.
type FinalizedSideEffectsOptions struct {
	ProjectDir string
	DataDir    string
}

// RunFinalizedSideEffects runs notifications and GitHub reconciliation after
// an external analysis backend has finalized dashboard output.
func RunFinalizedSideEffects(ctx context.Context, opts FinalizedSideEffectsOptions) error {
	cfg, err := project.Load(filepath.Join(opts.ProjectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	details, flakiness, err := loadFinalizedData(opts.DataDir)
	if err != nil {
		return err
	}
	backend, err := storage.New(cfg.StorageConfig(), &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("configuring storage: %w", err)
	}
	provider := cfg.ResolveAIProvider(os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"))
	aiToken := os.Getenv("AI_TOKEN")
	enableAI := aiToken != "" && provider.Endpoint != "" && provider.Model != ""
	client := &http.Client{Timeout: 30 * time.Second}
	p := &pipeline{
		opts: Options{ProjectDir: opts.ProjectDir, OutDir: opts.DataDir},
		cfg:  cfg, client: client, backend: backend,
		enableAI: enableAI, aiToken: aiToken,
	}
	return p.runSideEffects(ctx, &refreshResult{details: details, flakiness: flakiness})
}

func loadFinalizedData(dataDir string) ([]models.JobDetail, models.FlakinessReport, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "jobs", "*.json"))
	if err != nil {
		return nil, models.FlakinessReport{}, fmt.Errorf("list finalized jobs: %w", err)
	}
	sort.Strings(paths)
	details := make([]models.JobDetail, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, models.FlakinessReport{}, fmt.Errorf("read finalized job %s: %w", path, err)
		}
		var detail models.JobDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			return nil, models.FlakinessReport{}, fmt.Errorf("parse finalized job %s: %w", path, err)
		}
		if strings.TrimSpace(detail.JobID) == "" {
			return nil, models.FlakinessReport{}, fmt.Errorf("finalized job %s has no job_id", path)
		}
		details = append(details, detail)
	}

	path := filepath.Join(dataDir, "flakiness.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, models.FlakinessReport{}, fmt.Errorf("read finalized flakiness: %w", err)
	}
	var report models.FlakinessReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, models.FlakinessReport{}, fmt.Errorf("parse finalized flakiness: %w", err)
	}
	return details, report, nil
}
