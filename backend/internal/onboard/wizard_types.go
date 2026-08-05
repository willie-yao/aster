package onboard

import (
	"context"
	"io"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// DeploymentPlan describes the selected first-run deployment profile.
type DeploymentPlan struct {
	Mode      string `json:"mode"`
	AIEnabled bool   `json:"ai_enabled"`
	AIAPI     string `json:"ai_api,omitempty"`
	Endpoint  string `json:"ai_endpoint,omitempty"`
	Model     string `json:"ai_model,omitempty"`
}

// DiscoveryPlan records the selected source and completed real job sweep.
type DiscoveryPlan struct {
	TestGrid           string              `json:"testgrid,omitempty"`
	Bucket             string              `json:"bucket,omitempty"`
	GCSWebBase         string              `json:"gcsweb_base,omitempty"`
	CatalogRevision    string              `json:"catalog_revision,omitempty"`
	Jobs               []models.ProwJob    `json:"jobs"`
	SelectedCandidate  *DashboardCandidate `json:"selected_candidate,omitempty"`
	TestGridProvenance *Inferred[string]   `json:"testgrid_provenance,omitempty"`
}

// PromptPlan describes the generated prompt without carrying provider secrets.
type PromptPlan struct {
	RequestedMode   string `json:"requested_mode"`
	FinalStatus     string `json:"final_status"`
	Output          string `json:"output"`
	Source          string `json:"source"`
	FailureStage    string `json:"failure_stage,omitempty"`
	FailureCategory string `json:"failure_category,omitempty"`
	FailureAction   string `json:"failure_action,omitempty"`
	API             string `json:"api,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	Model           string `json:"model,omitempty"`
	Timeout         string `json:"timeout,omitempty"`
}

const (
	destinationActionCreate  = "create"
	destinationActionReplace = "replace"
)

// DestinationFilePlan describes one reviewed local scaffold mutation.
type DestinationFilePlan struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// DestinationPlan describes the only mutation the apply phase may perform.
type DestinationPlan struct {
	OutDir         string                `json:"out_dir,omitempty"`
	OpenPR         bool                  `json:"open_pr"`
	UpdateExisting bool                  `json:"update_existing,omitempty"`
	Files          []DestinationFilePlan `json:"files,omitempty"`
	StaleFiles     []string              `json:"stale_files,omitempty"`
}

// Plan is a complete credential-free onboarding plan.
type Plan struct {
	SourceRepo    Repo                        `json:"source_repo"`
	DashboardRepo Repo                        `json:"dashboard_repo"`
	Deployment    DeploymentPlan              `json:"deployment"`
	Discovery     DiscoveryPlan               `json:"discovery"`
	Project       project.Config              `json:"project"`
	Prompt        PromptPlan                  `json:"prompt"`
	Destination   DestinationPlan             `json:"destination"`
	Provenance    map[string]Inferred[string] `json:"provenance,omitempty"`
	Files         map[string]string           `json:"-"`
}

// Terminal supplies injected input and output for the interactive wizard.
type Terminal struct {
	In          io.Reader
	Out         io.Writer
	Err         io.Writer
	Interactive bool
}

// jobSweeper runs the same final artifact or TestGrid discovery as the fetcher.
type jobSweeper interface {
	Discover(context.Context, *project.Config, bool) ([]models.ProwJob, error)
}

// remoteDetector reads the current checkout's GitHub origin, when present.
type remoteDetector interface {
	Origin(context.Context) (string, error)
	Root(context.Context) (string, error)
}

// promptBuilder renders or drafts prompts/system.md.
type promptBuilder interface {
	Build(context.Context, Options, scaffoldData, promptDraftInput) (string, promptPreparationResult, error)
}

// scaffoldWriter applies rendered files locally.
type scaffoldWriter interface {
	Inspect(string, map[string]string) ([]DestinationFilePlan, []string, error)
	Write(string, map[string]string, bool, []DestinationFilePlan) error
}

// pullRequestWriter applies rendered files through GitHub.
type pullRequestWriter interface {
	Open(context.Context, Repo, map[string]string, string, string, string, string) (string, error)
}
