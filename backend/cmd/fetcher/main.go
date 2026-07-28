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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
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
	analysisFlags := fetcher.BindAnalysisRuntimeFlags(flag.CommandLine, &opts)
	flag.Parse()

	if err := analysisFlags.DecodePlacement(&opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	opts.Version = version

	if err := fetcher.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runOnboard parses the onboard command and its read-only discover mode.
func runOnboard(args []string) {
	if len(args) > 0 && args[0] == "discover" {
		runOnboardDiscover(args[1:])
		return
	}

	fs := flag.NewFlagSet("onboard", flag.ExitOnError)
	var opts onboard.Options
	var enableAI bool
	var includePresubmits bool
	fs.StringVar(&opts.TestGrid, "testgrid", "", "testgrid dashboard name to discover jobs from (kubernetes-ecosystem Prow)")
	fs.StringVar(&opts.Bucket, "bucket", "", "artifact bucket name for bucket-based discovery (any Prow); alternative to -testgrid")
	fs.StringVar(&opts.GCSWebBase, "gcsweb-base", "", "gcsweb gateway root for the bucket (for example, https://gcsweb.istio.io/s3); selects the gcsweb provider")
	fs.StringVar(&opts.DashboardRepo, "dashboard-repo", "", "owner/name of the repo that will publish the dashboard")
	fs.StringVar(&opts.SourceRepo, "source-repo", "", "source repo as owner/name or a GitHub URL; defaults to the current origin in the wizard")
	fs.StringVar(&opts.Mode, "mode", "", "deploy target: pages (GitHub Actions + Pages) or k8s (Kubernetes-native Helm)")
	fs.StringVar(&opts.ID, "id", "", "project id (default: derived from repository metadata)")
	fs.StringVar(&opts.Name, "name", "", "project display name (default: derived from repository metadata)")
	fs.StringVar(&opts.ShortName, "short-name", "", "short display name (optional)")
	fs.BoolVar(&includePresubmits, "include-presubmits", false, "include presubmit jobs in the sweep")
	fs.StringVar(&opts.EngineRef, "engine-ref", "main", "prow-ai-dashboard ref the generated workflows pin")
	fs.StringVar(&opts.OutDir, "out", "", "output directory for the scaffold (default: the dashboard repo name)")
	fs.BoolVar(&enableAI, "ai", true, "enable deployed AI failure analysis")
	fs.BoolVar(&opts.NoPrompt, "no-prompt", false, "skip AI prompt drafting and always write the prompts/system.md stub")
	fs.BoolVar(&opts.OpenPR, "open-pr", false, "open a PR against the dashboard repo instead of writing locally; needs GITHUB_TOKEN write access")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "discover, render, and validate without writing files or opening a pull request")
	fs.BoolVar(&opts.NonInteractive, "non-interactive", false, "forbid prompts and require all necessary flags")
	_ = fs.Parse(args)

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "include-presubmits":
			opts.IncludePresubmits = &includePresubmits
		case "ai":
			opts.AIEnabled = &enableAI
		}
	})

	// These variables configure prompt drafting and seed the deployed provider.
	// Tokens remain environment-only and are never copied into the plan.
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

func runOnboardDiscover(args []string) {
	fs := flag.NewFlagSet("onboard discover", flag.ExitOnError)
	var sourceRepo string
	var jsonOutput bool
	fs.StringVar(&sourceRepo, "source-repo", "", "source repo as owner/name or a GitHub URL; defaults to the current origin")
	fs.BoolVar(&jsonOutput, "json", false, "write the discovery report as JSON")
	_ = fs.Parse(args)

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 5*time.Minute)
	defer cancel()

	report, err := onboard.Discover(ctx, sourceRepo, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := onboard.WriteDiscovery(os.Stdout, report, jsonOutput); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
