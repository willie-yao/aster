// Command fetcher is the dashboard data pipeline. It loads a project
// configuration, discovers Prow jobs, fetches build results from GCS, runs
// optional AI failure analysis, and writes JSON for the frontend to render.
// Orchestration lives in internal/fetcher; this file handles flags.
//
// The onboard subcommand scaffolds a new dashboard config from a TestGrid
// dashboard name or storage bucket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard"
)

// version is the engine version. Builds can override it with
// -ldflags "-X main.version=<tag>"; local builds use "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "onboard" {
		runOnboard(os.Args[2:])
		return
	}

	var opts fetcher.Options
	flag.StringVar(&opts.ProjectDir, "project-dir", ".", "directory containing project.yaml and prompts/system.md")
	flag.StringVar(&opts.OutDir, "out", "data", "output directory for JSON files")
	flag.IntVar(&opts.BuildsPerJob, "builds", 10, "number of recent builds to fetch per job")
	flag.IntVar(&opts.Workers, "workers", 5, "number of concurrent job fetchers")
	flag.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "overall fetch timeout")
	flag.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in addition to periodics (ORed with project.yaml source.include_presubmits)")
	flag.BoolVar(&opts.EnableAI, "ai", false, "enable AI-powered failure analysis")
	flag.BoolVar(&opts.SkipSideEffects, "skip-side-effects", false, "write data without notifications, issues, or fix PRs")
	flag.StringVar(&opts.AnalysisRuntime.Type, "analysis-runtime", fetcher.AnalysisRuntimeInProcess, "single-failure analysis runtime: inprocess or orka-container")
	container := &opts.AnalysisRuntime.OrkaContainer
	flag.StringVar(&container.Namespace, "orka-analysis-namespace", "", "Orka namespace for container analysis Tasks")
	flag.StringVar(&container.ResultAPI, "orka-analysis-api", "", "Orka result API base URL")
	flag.StringVar(&container.Image, "orka-analysis-image", "", "analyzer image for Orka container Tasks")
	flag.StringVar(&container.ModelSecretName, "orka-analysis-model-secret", "", "model token Secret in the Orka namespace")
	flag.StringVar(&container.ModelTokenKey, "orka-analysis-model-token-key", "token", "model token Secret key")
	flag.StringVar(&container.StateSecretName, "orka-analysis-state-secret", "", "state key Secret name in the dashboard and Orka namespaces")
	flag.StringVar(&container.StateSecretKey, "orka-analysis-state-key", "state-key", "state Secret key")
	flag.IntVar(&container.MaxConcurrent, "orka-analysis-max-concurrent-tasks", 2, "maximum concurrent Orka analysis Tasks")
	flag.DurationVar(&container.PollInterval, "orka-analysis-poll-interval", 2*time.Second, "Orka Task poll interval")
	flag.DurationVar(&container.TaskTimeout, "orka-analysis-task-timeout", 20*time.Minute, "per-Task timeout")
	flag.IntVar(&container.Retries, "orka-analysis-retries", 1, "Orka Task retries")
	var nodeSelectorJSON, tolerationsJSON, affinityJSON string
	flag.StringVar(&nodeSelectorJSON, "orka-analysis-node-selector-json", "", "JSON node selector for analyzer Tasks")
	flag.StringVar(&tolerationsJSON, "orka-analysis-tolerations-json", "", "JSON tolerations for analyzer Tasks")
	flag.StringVar(&affinityJSON, "orka-analysis-affinity-json", "", "JSON affinity for analyzer Tasks")
	flag.Parse()

	if err := decodeAnalysisPlacement(nodeSelectorJSON, tolerationsJSON, affinityJSON, container); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	opts.Version = version

	if err := fetcher.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func decodeAnalysisPlacement(nodeSelectorJSON, tolerationsJSON, affinityJSON string, opts *fetcher.OrkaContainerAnalysisOptions) error {
	if nodeSelectorJSON != "" {
		if err := json.Unmarshal([]byte(nodeSelectorJSON), &opts.NodeSelector); err != nil {
			return fmt.Errorf("parse Orka analysis node selector: %w", err)
		}
	}
	if tolerationsJSON != "" {
		if err := json.Unmarshal([]byte(tolerationsJSON), &opts.Tolerations); err != nil {
			return fmt.Errorf("parse Orka analysis tolerations: %w", err)
		}
	}
	if affinityJSON != "" {
		if err := json.Unmarshal([]byte(affinityJSON), &opts.Affinity); err != nil {
			return fmt.Errorf("parse Orka analysis affinity: %w", err)
		}
	}
	return nil
}

// runOnboard parses the onboard subcommand flags and scaffolds a new dashboard.
func runOnboard(args []string) {
	fs := flag.NewFlagSet("onboard", flag.ExitOnError)
	var opts onboard.Options
	fs.StringVar(&opts.TestGrid, "testgrid", "", "testgrid dashboard name to discover jobs from (kubernetes-ecosystem Prow)")
	fs.StringVar(&opts.Bucket, "bucket", "", "artifact bucket name for bucket-based discovery (any Prow); alternative to -testgrid")
	fs.StringVar(&opts.GCSWebBase, "gcsweb-base", "", "gcsweb gateway root for the bucket (e.g. https://gcsweb.istio.io/s3); selects the gcsweb provider")
	fs.StringVar(&opts.DashboardRepo, "dashboard-repo", "", "owner/name of the repo that will publish the dashboard (required)")
	fs.StringVar(&opts.SourceRepo, "source-repo", "", "owner/name of the code repo under test (required)")
	fs.StringVar(&opts.Mode, "mode", "pages", "deploy target for the scaffold: \"pages\" (GitHub Actions + Pages) or \"k8s\" (Kubernetes-native Helm)")
	fs.StringVar(&opts.ID, "id", "", "project id (default: derived from the dashboard repo name)")
	fs.StringVar(&opts.Name, "name", "", "project display name (default: derived from the id)")
	fs.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in the sweep")
	fs.StringVar(&opts.EngineRef, "engine-ref", "main", "prow-ai-dashboard ref the generated workflows pin")
	fs.StringVar(&opts.OutDir, "out", "", "output directory for the scaffold (default: the dashboard repo name)")
	fs.BoolVar(&opts.NoPrompt, "no-prompt", false, "skip AI prompt drafting and always write the prompts/system.md stub")
	fs.BoolVar(&opts.OpenPR, "open-pr", false, "open a PR against the dashboard repo with the scaffold instead of writing a local directory (needs GITHUB_TOKEN write access)")
	_ = fs.Parse(args)

	// AI_TOKEN authenticates the chat-completions endpoint for prompt drafting.
	// GITHUB_TOKEN reads the source repo's docs.
	opts.AIToken = os.Getenv("AI_TOKEN")
	opts.AIAPI = os.Getenv("AI_API")
	opts.AIEndpoint = os.Getenv("AI_ENDPOINT")
	opts.AIModel = os.Getenv("AI_MODEL")
	opts.GitHubToken = os.Getenv("GITHUB_TOKEN")

	if err := onboard.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
