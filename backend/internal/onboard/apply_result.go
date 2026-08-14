package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	applyResultSchemaVersion  = 1
	setupHandoffSchemaVersion = 1
	maxArtifactSmokeBuilds    = 5
)

// ReviewedApplyOptions configures post-apply validation and handoff outputs.
type ReviewedApplyOptions struct {
	PlanDigest          string
	ResultOut           string
	HandoffOut          string
	ArtifactSmokeBuilds int
}

// ApplyResult is the deterministic post-apply file manifest.
type ApplyResult struct {
	SchemaVersion       int                `json:"schema_version"`
	PlanDigest          string             `json:"plan_digest"`
	Destination         string             `json:"destination"`
	Files               []AppliedFile      `json:"files"`
	MatchesReviewedPlan bool               `json:"matches_reviewed_plan"`
	Prompt              AppliedPromptState `json:"prompt"`
}

// AppliedFile records one created, replaced, or preserved consumer file.
type AppliedFile struct {
	Path                string `json:"path"`
	Mode                string `json:"mode"`
	SHA256              string `json:"sha256"`
	Status              string `json:"status"`
	Ownership           string `json:"ownership"`
	MatchesReviewedPlan bool   `json:"matches_reviewed_plan"`
}

// AppliedPromptState separates the original, candidate, and active prompt hashes.
type AppliedPromptState struct {
	OriginalSHA256  string `json:"original_sha256,omitempty"`
	CandidateSHA256 string `json:"candidate_sha256"`
	ActiveSHA256    string `json:"active_sha256"`
	Status          string `json:"status"`
}

// SetupPromptHandoff carries the active state and source-only candidate for the next phase.
type SetupPromptHandoff struct {
	AppliedPromptState
	BaselineStatus      string `json:"baseline_status"`
	ActivePath          string `json:"active_path"`
	SourceOnlyCandidate string `json:"source_only_candidate"`
	RequestedMode       string `json:"requested_mode"`
	Source              string `json:"source"`
}

// SetupHandoff is the machine-readable boundary for diagnostic authoring.
type SetupHandoff struct {
	SchemaVersion    int                     `json:"schema_version"`
	PlanDigest       string                  `json:"plan_digest"`
	Engine           EnginePlan              `json:"engine"`
	Consumer         ConsumerHandoff         `json:"consumer"`
	Source           SourceHandoff           `json:"source"`
	Discovery        DiscoveryHandoff        `json:"discovery"`
	ArtifactLocation ArtifactLocationHandoff `json:"artifact_location"`
	TestInfra        TestInfraHandoff        `json:"test_infra"`
	Deployment       DeploymentPlan          `json:"deployment"`
	Prompt           SetupPromptHandoff      `json:"prompt"`
	ApplyResult      ApplyResult             `json:"apply_result"`
	ArtifactSmoke    ArtifactSmokeReport     `json:"artifact_smoke"`
	Doctor           DoctorReport            `json:"doctor"`
	Warnings         []string                `json:"unresolved_warnings,omitempty"`
	NextPhase        string                  `json:"next_phase"`
}

// ConsumerHandoff identifies the generated consumer.
type ConsumerHandoff struct {
	Repository Repo   `json:"repository"`
	Path       string `json:"path"`
	ProjectID  string `json:"project_id"`
	Name       string `json:"name"`
}

// SourceHandoff pins the source repository and revision.
type SourceHandoff struct {
	Repository Repo               `json:"repository"`
	Revision   SourceRevisionPlan `json:"revision"`
}

// DiscoveryHandoff records the exact selector, catalog, digest, and jobs.
type DiscoveryHandoff struct {
	Selector        string           `json:"selector"`
	TestGrid        string           `json:"testgrid,omitempty"`
	Bucket          string           `json:"bucket,omitempty"`
	GCSWebBase      string           `json:"gcsweb_base,omitempty"`
	ExactJobs       []string         `json:"exact_jobs,omitempty"`
	CatalogRevision string           `json:"catalog_revision,omitempty"`
	Digest          string           `json:"digest"`
	Jobs            []models.ProwJob `json:"jobs"`
}

// ArtifactLocationHandoff records the storage coordinates used by selected jobs.
type ArtifactLocationHandoff struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket,omitempty"`
	Base     string `json:"base,omitempty"`
	WebBase  string `json:"web_base,omitempty"`
	ProwBase string `json:"prow_base,omitempty"`
}

// TestInfraHandoff records whether current job configuration came from test-infra.
type TestInfraHandoff struct {
	Repository  *Repo    `json:"repository,omitempty"`
	Revision    string   `json:"revision,omitempty"`
	Status      string   `json:"status"`
	ConfigFiles []string `json:"config_files,omitempty"`
}

// ApplyReviewed applies a reviewed local plan, validates it, and writes optional handoff outputs.
func ApplyReviewed(ctx context.Context, plan *Plan, githubToken string, opts ReviewedApplyOptions) (ApplyResult, SetupHandoff, error) {
	var emptyResult ApplyResult
	var emptyHandoff SetupHandoff
	if plan == nil {
		return emptyResult, emptyHandoff, fmt.Errorf("onboarding plan is nil")
	}
	if plan.Destination.OpenPR {
		return emptyResult, emptyHandoff, fmt.Errorf("reviewed apply results require a local destination")
	}
	if _, err := parsePlanArtifactDigest(opts.PlanDigest); err != nil {
		return emptyResult, emptyHandoff, err
	}
	if plan.reviewedDigest == "" || plan.reviewedDigest != strings.ToLower(strings.TrimSpace(opts.PlanDigest)) {
		return emptyResult, emptyHandoff, fmt.Errorf("onboarding plan digest does not match the loaded reviewed artifact")
	}
	if opts.ArtifactSmokeBuilds < 0 || opts.ArtifactSmokeBuilds > maxArtifactSmokeBuilds {
		return emptyResult, emptyHandoff, fmt.Errorf("artifact smoke builds must be between 0 and %d", maxArtifactSmokeBuilds)
	}
	resultPath, handoffPath, err := prepareReviewedOutputPaths(plan.Destination.OutDir, opts.ResultOut, opts.HandoffOut)
	if err != nil {
		return emptyResult, emptyHandoff, err
	}

	terminal := Terminal{In: strings.NewReader(""), Out: os.Stdout, Err: os.Stderr}
	deps := defaultDependencies(Options{GitHubToken: githubToken}, terminal)
	if err := applyPlan(ctx, plan, githubToken, deps); err != nil {
		return emptyResult, emptyHandoff, err
	}
	result, err := buildApplyResult(plan, opts.PlanDigest)
	if err != nil {
		return emptyResult, emptyHandoff, err
	}
	doctor := Doctor(ctx, DoctorOptions{ProjectDir: plan.Destination.OutDir})
	smoke := runArtifactSmoke(ctx, plan, opts.ArtifactSmokeBuilds)
	handoff := buildSetupHandoff(plan, opts.PlanDigest, result, doctor, smoke)
	if resultPath != "" {
		if err := writePrivateJSON(resultPath, result); err != nil {
			return result, handoff, err
		}
	}
	if handoffPath != "" {
		if err := writePrivateJSON(handoffPath, handoff); err != nil {
			return result, handoff, err
		}
	}
	if doctor.HasFailures() {
		return result, handoff, fmt.Errorf("onboard doctor reported failures after apply")
	}
	return result, handoff, nil
}

func prepareReviewedOutputPaths(destination, resultOut, handoffOut string) (string, string, error) {
	canonicalDestination, err := canonicalLocalDestination(destination)
	if err != nil {
		return "", "", err
	}
	paths := []string{strings.TrimSpace(resultOut), strings.TrimSpace(handoffOut)}
	resolved := make([]string, len(paths))
	for i, raw := range paths {
		if raw == "" {
			continue
		}
		path, err := canonicalPlanArtifactPath(raw)
		if err != nil {
			return "", "", err
		}
		inside, err := pathWithinDirectory(path, canonicalDestination)
		if err != nil {
			return "", "", fmt.Errorf("compare reviewed output and destination paths: %w", err)
		}
		if inside {
			return "", "", fmt.Errorf("reviewed output files must be outside the dashboard consumer directory")
		}
		if _, err := os.Lstat(path); err == nil {
			return "", "", fmt.Errorf("reviewed output file already exists: %s", path)
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("inspect reviewed output file: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", "", fmt.Errorf("create reviewed output directory: %w", err)
		}
		resolved[i] = path
	}
	if resolved[0] != "" && resolved[0] == resolved[1] {
		return "", "", fmt.Errorf("apply result and setup handoff paths must differ")
	}
	return resolved[0], resolved[1], nil
}

func buildApplyResult(plan *Plan, planDigest string) (ApplyResult, error) {
	result := ApplyResult{
		SchemaVersion: applyResultSchemaVersion, PlanDigest: planDigest,
		Destination: plan.Destination.OutDir, MatchesReviewedPlan: true,
		Prompt: AppliedPromptState{
			OriginalSHA256: plan.Prompt.ExistingSHA256, CandidateSHA256: plan.Prompt.CandidateSHA256,
		},
	}
	files := append([]DestinationFilePlan(nil), plan.Destination.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, reviewed := range files {
		full := filepath.Join(plan.Destination.OutDir, filepath.FromSlash(reviewed.Path))
		info, err := os.Lstat(full)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("inspect applied file %s: %w", reviewed.Path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ApplyResult{}, fmt.Errorf("applied file %s is not a regular file", reviewed.Path)
		}
		digest, err := digestDestinationFile(full)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("hash applied file %s: %w", reviewed.Path, err)
		}
		expected := reviewed.ReviewedDigest
		if reviewed.Action != destinationActionPreserve {
			expected = planArtifactDigest([]byte(plan.Files[reviewed.Path]))
		}
		matches := digest == expected
		result.Files = append(result.Files, AppliedFile{
			Path: reviewed.Path, Mode: fmt.Sprintf("%#o", info.Mode().Perm()), SHA256: digest,
			Status: reviewed.Action, Ownership: reviewed.Ownership, MatchesReviewedPlan: matches,
		})
		result.MatchesReviewedPlan = result.MatchesReviewedPlan && matches
		if reviewed.Path == "prompts/system.md" {
			result.Prompt.ActiveSHA256 = digest
			result.Prompt.Status = promptApplyStatus(reviewed.Action)
		}
	}
	if result.Prompt.ActiveSHA256 == "" {
		return ApplyResult{}, fmt.Errorf("applied prompt was not recorded")
	}
	if !result.MatchesReviewedPlan {
		return result, fmt.Errorf("applied result does not match the reviewed plan")
	}
	return result, nil
}

func promptApplyStatus(action string) string {
	switch action {
	case destinationActionPreserve:
		return "preserved-existing"
	case destinationActionReplace:
		return "replaced-with-source-only-baseline"
	default:
		return "created-source-only-baseline"
	}
}

func buildSetupHandoff(plan *Plan, planDigest string, result ApplyResult, doctor DoctorReport, smoke ArtifactSmokeReport) SetupHandoff {
	warnings := append([]string(nil), plan.Warnings...)
	for _, check := range doctor.Checks {
		if check.Status == DoctorWarn || check.Status == DoctorFail {
			warnings = append(warnings, fmt.Sprintf("Doctor %s: %s", check.Name, check.Detail))
		}
	}
	warnings = append(warnings, smoke.Warnings...)
	warnings = dedupeSortedStrings(warnings)
	return SetupHandoff{
		SchemaVersion: setupHandoffSchemaVersion,
		PlanDigest:    planDigest,
		Engine:        plan.Engine,
		Consumer:      ConsumerHandoff{Repository: plan.DashboardRepo, Path: plan.Destination.OutDir, ProjectID: plan.Project.ID, Name: plan.Project.Name},
		Source:        SourceHandoff{Repository: plan.SourceRepo, Revision: plan.SourceRevision},
		Discovery: DiscoveryHandoff{
			Selector: discoverySelector(plan.Discovery), TestGrid: plan.Discovery.TestGrid, Bucket: plan.Discovery.Bucket,
			GCSWebBase: plan.Discovery.GCSWebBase, ExactJobs: append([]string(nil), plan.Discovery.ExactJobs...),
			CatalogRevision: plan.Discovery.CatalogRevision, Digest: plan.Discovery.Digest,
			Jobs: append([]models.ProwJob(nil), plan.Discovery.Jobs...),
		},
		ArtifactLocation: artifactLocationHandoff(plan),
		TestInfra:        testInfraHandoff(plan),
		Deployment:       plan.Deployment, Prompt: SetupPromptHandoff{
			AppliedPromptState: result.Prompt, BaselineStatus: plan.Prompt.BaselineStatus,
			ActivePath:          filepath.Join(plan.Destination.OutDir, "prompts", "system.md"),
			SourceOnlyCandidate: plan.Files["prompts/system.md"], RequestedMode: plan.Prompt.RequestedMode, Source: plan.Prompt.Source,
		}, ApplyResult: result,
		ArtifactSmoke: smoke, Doctor: doctor, Warnings: warnings,
		NextPhase: "Run $author-aster-diagnostics with this handoff. The active prompt is a source-only baseline and has not been validated against historical failures.",
	}
}

func artifactLocationHandoff(plan *Plan) ArtifactLocationHandoff {
	return ArtifactLocationHandoff{
		Provider: plan.Project.Storage.Provider,
		Bucket:   plan.Project.Storage.Bucket,
		Base:     plan.Project.Storage.Base,
		WebBase:  plan.Project.Storage.WebBase,
		ProwBase: plan.Project.Storage.ProwBase,
	}
}

func testInfraHandoff(plan *Plan) TestInfraHandoff {
	configFiles := make([]string, 0, len(plan.Discovery.Jobs))
	seen := map[string]struct{}{}
	for _, job := range plan.Discovery.Jobs {
		if job.ConfigFile == "" {
			continue
		}
		if _, ok := seen[job.ConfigFile]; ok {
			continue
		}
		seen[job.ConfigFile] = struct{}{}
		configFiles = append(configFiles, job.ConfigFile)
	}
	sort.Strings(configFiles)
	if plan.Discovery.TestGrid == "" {
		return TestInfraHandoff{Status: "not_applicable", ConfigFiles: configFiles}
	}
	repository := &Repo{Owner: "kubernetes", Name: "test-infra", FullName: "kubernetes/test-infra"}
	status := sourceRevisionUnresolved
	if validGitRevision(plan.Discovery.CatalogRevision) {
		status = sourceRevisionResolved
	}
	return TestInfraHandoff{
		Repository: repository, Revision: plan.Discovery.CatalogRevision,
		Status: status, ConfigFiles: configFiles,
	}
}

func discoverySelector(plan DiscoveryPlan) string {
	if plan.TestGrid != "" {
		return "testgrid"
	}
	if len(plan.ExactJobs) > 0 {
		return "bucket-exact-jobs"
	}
	return "bucket"
}

func dedupeSortedStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal reviewed output: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create reviewed output %s: %w", path, err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write reviewed output %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync reviewed output %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close reviewed output %s: %w", path, err)
	}
	complete = true
	return nil
}
