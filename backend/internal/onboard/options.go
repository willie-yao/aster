package onboard

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

	// ID, Name, and ShortName override the derived project identity. Optional.
	ID        string
	Name      string
	ShortName string

	// IncludePresubmits widens the sweep to presubmit jobs. Nil means the
	// interactive wizard may ask; non-interactive planning treats nil as false.
	IncludePresubmits *bool

	// EngineRef is the prow-ai-dashboard ref the generated workflows pin.
	EngineRef string

	// OutDir is where the scaffold is written.
	OutDir string

	// AIEnabled controls deployed failure analysis. Nil preserves the existing
	// enabled-by-default scaffold behavior.
	AIEnabled *bool

	// AI prompt drafting is optional.

	// AIToken authenticates the provider used to draft prompts/system.md. It is
	// never copied into a plan or generated file.
	AIToken string
	// AIAPI selects chat_completions (default) or responses.
	AIAPI string
	// AIEndpoint and AIModel identify the provider used for prompt drafting and
	// seed deployed provider settings when explicitly confirmed by the wizard.
	AIEndpoint string
	AIModel    string

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
	// NoPrompt forces the stub even when an AI token is available.
	NoPrompt bool
	// PromptDebug writes sanitized prompt-preparation diagnostics to stderr.
	PromptDebug bool
	// RequirePromptDraft fails before writes unless experimental API drafting succeeds.
	RequirePromptDraft bool

	// OpenPR opens a pull request against the dashboard repo with the scaffold
	// instead of writing a local directory. Requires a GitHub token with write
	// access to the dashboard repo.
	OpenPR bool

	// DryRun performs discovery, planning, rendering, and validation without
	// writing files or opening a pull request.
	DryRun bool
	// NonInteractive forbids terminal reads even when stdin is a TTY.
	NonInteractive bool
}
