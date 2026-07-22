package onboard

// Options configures a scaffold run. One discovery selector is required, plus
// the dashboard and source repos.
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
	// SourceRepo is the owner/name repo for the code under test.
	SourceRepo string

	// Mode selects the deploy target the scaffold is generated for:
	// "pages" (GitHub Actions + Pages, the default) or "k8s" (Kubernetes-native
	// Helm). It changes which deploy files are emitted and the branding
	// defaults; project.yaml and prompts/system.md are the same either way.
	Mode string

	// ID/Name override the derived project identity. Optional.
	ID   string
	Name string

	// IncludePresubmits widens the sweep to presubmit jobs.
	IncludePresubmits bool

	// EngineRef is the prow-ai-dashboard ref the generated workflows pin.
	EngineRef string

	// OutDir is where the scaffold is written.
	OutDir string

	// AI prompt drafting is optional.

	// AIToken authenticates the chat-completions endpoint used to draft
	// prompts/system.md. When empty, onboard writes the stub instead.
	AIToken string
	// AIAPI selects chat_completions (default) or responses.
	AIAPI string
	// AIEndpoint / AIModel are the endpoint URL and model id of the
	// provider used to draft prompts/system.md. Both are required when AIToken
	// is set; the engine assumes no default provider.
	AIEndpoint string
	AIModel    string
	// GitHubToken authenticates source-repo doc reads and scaffold PR creation.
	GitHubToken string
	// NoPrompt forces the stub even when an AI token is available.
	NoPrompt bool

	// OpenPR opens a pull request against the dashboard repo with the scaffold
	// instead of writing a local directory. Requires a GitHub token with write
	// access to the dashboard repo.
	OpenPR bool
}
