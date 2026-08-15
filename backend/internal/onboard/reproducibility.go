package onboard

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	sourceRevisionResolved   = "resolved"
	sourceRevisionUnresolved = "unresolved"
)

type githubSourceRevisionResolver struct{}

func (githubSourceRevisionResolver) Resolve(ctx context.Context, repo Repo, token string) (SourceRevisionPlan, error) {
	client := defaultDiscoveryHTTPClient()
	branch := strings.TrimSpace(repo.Branch)
	if branch == "" {
		var err error
		branch, err = defaultBranch(ctx, client, repo.Owner, repo.Name, token)
		if err != nil {
			return SourceRevisionPlan{Status: sourceRevisionUnresolved}, err
		}
	}
	revision, err := resolvePromptSourceRevision(ctx, client, repo.Owner, repo.Name, branch, token)
	if err != nil {
		return SourceRevisionPlan{Ref: branch, Status: sourceRevisionUnresolved}, err
	}
	return SourceRevisionPlan{Revision: revision, Ref: branch, Status: sourceRevisionResolved}, nil
}

func currentEnginePlan() EnginePlan {
	plan := EnginePlan{Version: "(devel)"}
	if info, ok := debug.ReadBuildInfo(); ok {
		plan.Path = info.Main.Path
		if info.Main.Version != "" {
			plan.Version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				plan.Revision = setting.Value
			case "vcs.modified":
				plan.Modified, _ = strconv.ParseBool(setting.Value)
			}
		}
	}
	if checkout := findEngineCheckout(); plan.Version == "(devel)" && checkout != "" {
		plan.Path = checkout
		if revision, err := gitOutput(checkout, "rev-parse", "HEAD"); err == nil && revision != "" {
			plan.Revision = revision
		}
		if status, err := gitOutput(checkout, "status", "--porcelain"); err == nil {
			plan.Modified = status != ""
		}
	}
	return plan
}

func findEngineCheckout() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	current := filepath.Clean(cwd)
	for {
		goMod := filepath.Join(current, "backend", "go.mod")
		if data, err := os.ReadFile(goMod); err == nil && strings.Contains(string(data), "module github.com/willie-yao/aster/backend") {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func gitOutput(root string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", root}, args...)
	out, err := exec.Command("git", cmdArgs...).Output()
	return strings.TrimSpace(string(out)), err
}

func discoveryPlanDigest(plan DiscoveryPlan) (string, error) {
	payload := struct {
		TestGrid        string           `json:"testgrid,omitempty"`
		Bucket          string           `json:"bucket,omitempty"`
		GCSWebBase      string           `json:"gcsweb_base,omitempty"`
		ExactJobs       []string         `json:"exact_jobs,omitempty"`
		CatalogRevision string           `json:"catalog_revision,omitempty"`
		Jobs            []models.ProwJob `json:"jobs"`
	}{
		TestGrid: plan.TestGrid, Bucket: plan.Bucket, GCSWebBase: plan.GCSWebBase,
		ExactJobs: append([]string(nil), plan.ExactJobs...), CatalogRevision: plan.CatalogRevision,
		Jobs: append([]models.ProwJob(nil), plan.Jobs...),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return planArtifactDigest(data), nil
}
