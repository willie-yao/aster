package onboard

import "time"

// Options configures a scaffold run. Complete flag-based runs require one
// discovery selector plus the dashboard and source repositories. Interactive
// runs may infer or prompt for missing values.
type Options struct {
	// TestGrid is the testgrid-dashboards annotation value for Kubernetes Prow.
	// Mutually exclusive with Bucket.
	TestGrid string
	// Bucket is the artifact bucket name for bucket-based discovery.
	Bucket string
	// GCSWebBase selects the gcsweb provider for bucket discovery.
	// Empty means native gcs.
	GCSWebBase string
	// ExactJobs selects exact bucket job indexes without bucket-root discovery.
	// It is valid only with Bucket and may be repeated on the CLI.
	ExactJobs []string

	// DashboardRepo is the owner/name repo that will publish the dashboard.
	DashboardRepo string
	// SourceRepo accepts owner/name or a GitHub repository URL for the code under
	// test. It is normalized to owner/name before planning.
	SourceRepo string

	// Mode selects the deploy target the scaffold is generated for:
	// "pages" (GitHub Actions + Pages, the default) or "k8s" (Kubernetes-native
	// Helm). It changes which deploy files are emitted and the branding
	// defaults; project.yaml and prompts/system.md are the same either way.
	Mode string
	// ModeReasons records the reviewed constraints that selected the deployment
	// mode. Values must not contain credentials.
	ModeReasons []string
	// ArtifactAccess records whether the selected Prow artifacts are public,
	// authenticated, private, or not yet established.
	ArtifactAccess string

	// K8sStorageClass and K8sExistingClaim select the shared ReadWriteMany
	// storage used by a Kubernetes scaffold. They are mutually exclusive.
	K8sStorageClass  string
	K8sExistingClaim string

	// ID, Name, and ShortName override the derived project identity. Optional.
	ID        string
	Name      string
	ShortName string

	// IncludePresubmits widens the sweep to presubmit jobs. Nil means the
	// interactive wizard may ask; non-interactive planning treats nil as false.
	IncludePresubmits *bool

	// EngineRef is the Aster ref the generated workflows pin.
	EngineRef string

	// OutDir is where the scaffold is written.
	OutDir string

	// AIEnabled controls deployed failure analysis. Nil preserves the existing
	// enabled-by-default scaffold behavior.
	AIEnabled *bool

	// PromptMode selects handoff or todo-template.
	PromptMode string

	// AIAPI selects chat_completions (default) or responses.
	AIAPI string
	// AIEndpoint and AIModel seed deployed provider settings for complete
	// flag-based runs.
	AIEndpoint string
	AIModel    string
	// AIToken is read only to ensure the deployment secret is not copied into a
	// generated file or other nonsecret field. Prompt authoring never sends it.
	AIToken string

	// DeploymentAIAPI, DeploymentAIEndpoint, and DeploymentAIModel are the
	// deployed dashboard provider selected by the wizard. Empty values preserve
	// existing flag-based behavior by falling back to AIAPI, AIEndpoint, and
	// AIModel.
	DeploymentAIAPI      string
	DeploymentAIEndpoint string
	DeploymentAIModel    string

	// deferDeploymentAI marks the wizard's configure-later choice without
	// changing flag-based AI-disabled provider seeding.
	deferDeploymentAI bool
	// GitHubToken authenticates metadata/doc reads and scaffold PR creation. It
	// is never copied into a plan or generated file.
	GitHubToken string
	// PromptTimeout bounds prompt authoring, including agent execution.
	PromptTimeout time.Duration

	// OpenPR opens a pull request against the dashboard repo with the scaffold
	// instead of writing a local directory. Requires a GitHub token with write
	// access to the dashboard repo.
	OpenPR bool
	// UpdateExisting permits replacement of known local scaffold files only.
	UpdateExisting bool
	// ReplaceConsumerOwned permits prompts/system.md replacement during a local
	// update. It requires UpdateExisting. Existing skills/*.yaml files are always
	// preserved.
	ReplaceConsumerOwned bool

	// DryRun performs discovery, planning, rendering, and validation without
	// applying scaffold files or opening a pull request.
	DryRun bool
	// PlanOut writes the exact reviewed dry-run plan to a new private file.
	PlanOut string
	// NonInteractive forbids terminal reads even when stdin is a TTY.
	NonInteractive bool

	allowK8sStoragePlaceholder bool
}
