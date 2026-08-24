// Command aster is the dashboard data pipeline. It loads a project
// configuration, discovers Prow jobs, fetches build results from GCS, runs
// optional AI failure analysis, and writes JSON for the frontend to render.
// Orchestration lives in internal/fetcher; this file handles flags.
//
// The onboard subcommand scaffolds a new dashboard config from a TestGrid
// dashboard name or storage bucket. The kubernetes subcommand validates and
// installs a consumer bundle with Helm. The notify-test subcommand sends one
// test email through the configured relay.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/credentialenv"
	"github.com/willie-yao/aster/backend/internal/fetcher"
	"github.com/willie-yao/aster/backend/internal/kubernetesdeploy"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/onboard"
	"github.com/willie-yao/aster/backend/internal/project"
)

// version is the engine version. Builds can override it with
// -ldflags "-X main.version=<tag>"; local builds use "dev".
var (
	version  = "dev"
	commit   = "dev"
	imageTag = "dev"
)

func main() {
	credentialenv.SanitizeAndReport()
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("aster version=%s commit=%s image_tag=%s\n", version, commit, imageTag)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "kubernetes" {
		runKubernetes(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "onboard" {
		runOnboard(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "notify-test" {
		runNotifyTest(os.Args[2:])
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
	flag.BoolVar(&opts.SkipSideEffects, "skip-side-effects", false, "write data without notifications, issue recovery, or pull request comments")
	fetcher.BindAnalysisRuntimeFlags(flag.CommandLine, &opts)
	flag.Parse()

	opts.Version = version
	opts.TraceEngine = ai.TraceEngine{Version: version, Commit: commit, ImageTag: imageTag}

	if err := fetcher.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runKubernetes(args []string) {
	if len(args) == 0 || (args[0] != "doctor" && args[0] != "gitops" && args[0] != "install" && args[0] != "upgrade") {
		fmt.Fprintln(os.Stderr, "error: usage: aster kubernetes <doctor|gitops|install|upgrade> [flags]")
		os.Exit(2)
	}
	if args[0] == "doctor" {
		runKubernetesDoctor(args[1:])
		return
	}
	if args[0] == "gitops" {
		runKubernetesGitOps(args[1:])
		return
	}

	opts := kubernetesdeploy.Options{Action: args[0]}
	fs := flag.NewFlagSet("kubernetes "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.ProjectDir, "project-dir", ".", "consumer directory containing project.yaml, prompts/system.md, and optional skills")
	fs.StringVar(&opts.ValuesFile, "values", filepath.Join("deploy", "values.yaml"), "Helm values file relative to project-dir unless absolute")
	fs.StringVar(&opts.Release, "release", "", "Helm release name (required)")
	fs.StringVar(&opts.Namespace, "namespace", "", "Kubernetes namespace (required)")
	fs.StringVar(&opts.KubeContext, "kube-context", "", "explicit Kubernetes context (required)")
	fs.StringVar(&opts.Chart, "chart", kubernetesdeploy.DefaultChart, "Helm chart path or OCI reference")
	fs.StringVar(&opts.ChartVersion, "chart-version", "", "optional OCI chart version")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "validate the bundle and render locally without cluster writes")
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := kubernetesdeploy.Run(ctx, opts, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runKubernetesGitOps(args []string) {
	if len(args) == 0 || (args[0] != "render" && args[0] != "check") {
		fmt.Fprintln(os.Stderr, "error: usage: aster kubernetes gitops <render|check> [flags]")
		os.Exit(2)
	}

	action := args[0]
	opts := kubernetesdeploy.GitOpsOptions{}
	fs := flag.NewFlagSet("kubernetes gitops "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.ProjectDir, "project-dir", ".", "consumer directory containing project.yaml, prompts/system.md, and optional skills")
	fs.StringVar(&opts.ValuesFile, "values", filepath.Join("deploy", "values.yaml"), "application Helm values file relative to project-dir")
	fs.StringVar(&opts.PlatformValuesFile, "platform-values", filepath.Join("deploy", "platform-values.yaml"), "platform Helm values file used when Agent Sandbox Fix is enabled")
	fs.StringVar(&opts.Release, "release", "", "application Helm release name (required)")
	fs.StringVar(&opts.Namespace, "namespace", "", "application and Flux control namespace (required)")
	fs.StringVar(&opts.ExecutionNamespace, "execution-namespace", "", "Agent Sandbox execution namespace (required when Fix is enabled)")
	fs.StringVar(&opts.Chart, "chart", kubernetesdeploy.DefaultChart, "application OCI chart reference")
	fs.StringVar(&opts.PlatformChart, "platform-chart", kubernetesdeploy.DefaultPlatformChart, "platform OCI chart reference")
	fs.StringVar(&opts.ChartVersion, "chart-version", "", "exact semantic chart version (required)")
	if action == "render" {
		fs.StringVar(&opts.OutputDir, "output", "gitops", "generated directory relative to project-dir")
		fs.BoolVar(&opts.DryRun, "dry-run", false, "validate and print the file plan without writing")
	} else {
		fs.StringVar(&opts.OutputDir, "gitops-dir", "gitops", "generated directory relative to project-dir")
	}
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}

	var err error
	if action == "render" {
		err = kubernetesdeploy.RenderGitOps(opts, os.Stdout)
	} else {
		err = kubernetesdeploy.CheckGitOps(opts, os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runKubernetesDoctor(args []string) {
	opts := kubernetesdeploy.KubernetesDoctorOptions{}
	fs := flag.NewFlagSet("kubernetes doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.Action, "action", "auto", "expected next operation: auto, install, or upgrade")
	fs.StringVar(&opts.ProjectDir, "project-dir", ".", "consumer directory containing project.yaml, prompts/system.md, and optional skills")
	fs.StringVar(&opts.ValuesFile, "values", filepath.Join("deploy", "values.yaml"), "Helm values file relative to project-dir unless absolute")
	fs.StringVar(&opts.Release, "release", "", "Helm release name (required)")
	fs.StringVar(&opts.Namespace, "namespace", "", "Kubernetes namespace (required)")
	fs.StringVar(&opts.KubeContext, "kube-context", "", "explicit Kubernetes context (required)")
	fs.StringVar(&opts.Chart, "chart", kubernetesdeploy.DefaultChart, "Helm chart path or OCI reference")
	fs.StringVar(&opts.ChartVersion, "chart-version", "", "optional OCI chart version")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report := kubernetesdeploy.KubernetesDoctor(ctx, opts)
	if err := kubernetesdeploy.WriteKubernetesDoctorReport(os.Stdout, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if report.HasFailures() {
		os.Exit(1)
	}
}

// runOnboard parses the onboard command and its read-only discover mode.
func runOnboard(args []string) {
	if len(args) > 0 && args[0] == "doctor" {
		runOnboardDoctor(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "discover" {
		runOnboardDiscover(args[1:])
		return
	}

	fs := flag.NewFlagSet("onboard", flag.ExitOnError)
	var opts onboard.Options
	var enableAI bool
	var includePresubmits bool
	var applyPlanPath string
	var applyPlanDigest string
	var applyResultOut string
	var setupHandoffOut string
	var artifactSmokeBuilds int
	fs.StringVar(&opts.TestGrid, "testgrid", "", "testgrid dashboard name to discover jobs from (kubernetes-ecosystem Prow)")
	fs.StringVar(&opts.Bucket, "bucket", "", "artifact bucket name for bucket-based discovery (any Prow); alternative to -testgrid")
	fs.StringVar(&opts.GCSWebBase, "gcsweb-base", "", "gcsweb gateway root for the bucket (for example, https://gcsweb.istio.io/s3); selects the gcsweb provider")
	fs.Func("exact-job", "exact periodic or postsubmit bucket job name; repeat as needed; requires -bucket", func(value string) error {
		opts.ExactJobs = append(opts.ExactJobs, value)
		return nil
	})
	fs.StringVar(&opts.DashboardRepo, "dashboard-repo", "", "owner/name of the repo that will publish the dashboard")
	fs.StringVar(&opts.SourceRepo, "source-repo", "", "source repo as owner/name or a GitHub URL; defaults to the current origin in the wizard")
	fs.StringVar(&opts.Mode, "mode", "", "deploy target: pages (GitHub Actions + Pages) or k8s (Kubernetes-native Helm)")
	fs.Func("deployment-reason", "reviewed reason for the deployment mode; repeat as needed", func(value string) error {
		opts.ModeReasons = append(opts.ModeReasons, value)
		return nil
	})
	fs.StringVar(&opts.ArtifactAccess, "artifact-access", "unknown", "artifact access: public, authenticated, private, or unknown")
	fs.StringVar(&opts.ID, "id", "", "project id (default: derived from repository metadata)")
	fs.StringVar(&opts.Name, "name", "", "project display name (default: derived from repository metadata)")
	fs.StringVar(&opts.ShortName, "short-name", "", "short display name (optional)")
	fs.BoolVar(&includePresubmits, "include-presubmits", false, "include presubmit jobs in the sweep")
	fs.StringVar(&opts.EngineRef, "engine-ref", "main", "Aster ref the generated workflows pin")
	fs.StringVar(&opts.OutDir, "out", "", "dashboard consumer directory for the scaffold")
	fs.BoolVar(&enableAI, "ai", true, "enable deployed AI failure analysis")
	fs.BoolVar(&opts.NoPrompt, "no-prompt", false, "skip prompt authoring and always write the prompts/system.md TODO template")
	fs.StringVar(&opts.PromptMode, "prompt-mode", "", "prompt authoring mode: handoff or todo-template")
	fs.DurationVar(&opts.PromptTimeout, "prompt-timeout", onboard.DefaultPromptDraftTimeout, "total timeout for prompt source resolution")
	fs.BoolVar(&opts.OpenPR, "open-pr", false, "open a PR against the dashboard repo instead of writing locally; needs GITHUB_TOKEN write access")
	fs.BoolVar(&opts.UpdateExisting, "update-existing", false, "replace only known engine-generated files in an existing local scaffold")
	fs.BoolVar(&opts.ReplaceConsumerOwned, "replace-consumer-owned", false, "with -update-existing, explicitly replace prompts/system.md; existing skills are always preserved")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "discover, render, and validate without applying scaffold files or opening a pull request")
	fs.StringVar(&opts.PlanOut, "plan-out", "", "write the exact reviewed dry-run plan to a new private file")
	fs.StringVar(&applyPlanPath, "apply-plan", "", "apply an exact reviewed plan artifact instead of rebuilding discovery")
	fs.StringVar(&applyPlanDigest, "plan-digest", "", "required sha256 digest for -apply-plan")
	fs.StringVar(&applyResultOut, "result-out", "", "write the deterministic post-apply file manifest outside the consumer")
	fs.StringVar(&setupHandoffOut, "handoff-out", "", "write the machine-readable diagnostic-authoring handoff outside the consumer")
	fs.IntVar(&artifactSmokeBuilds, "artifact-smoke-builds", 1, "recent builds per selected job for the read-only artifact usability check (0-5)")
	fs.BoolVar(&opts.NonInteractive, "non-interactive", false, "forbid prompts and require all necessary flags")
	_ = fs.Parse(args)

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
		switch f.Name {
		case "include-presubmits":
			opts.IncludePresubmits = &includePresubmits
		case "ai":
			opts.AIEnabled = &enableAI
		}
	})
	if applyPlanPath != "" {
		for name := range visited {
			switch name {
			case "apply-plan", "plan-digest", "result-out", "handoff-out", "artifact-smoke-builds":
				continue
			default:
				fmt.Fprintf(os.Stderr, "error: -apply-plan cannot be combined with -%s\n", name)
				os.Exit(2)
			}
		}
		if applyPlanDigest == "" {
			fmt.Fprintln(os.Stderr, "error: -apply-plan requires -plan-digest")
			os.Exit(2)
		}
		plan, err := onboard.ReadPlanArtifact(applyPlanPath, applyPlanDigest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		_, _, err = onboard.ApplyReviewed(context.Background(), plan, os.Getenv("GITHUB_TOKEN"), onboard.ReviewedApplyOptions{
			PlanDigest: applyPlanDigest, ResultOut: applyResultOut, HandoffOut: setupHandoffOut,
			ArtifactSmokeBuilds: artifactSmokeBuilds,
		})
		if applyResultOut != "" {
			if _, statErr := os.Stat(applyResultOut); statErr == nil {
				fmt.Fprintf(os.Stdout, "Apply result: %s\n", applyResultOut)
			}
		}
		if setupHandoffOut != "" {
			if _, statErr := os.Stat(setupHandoffOut); statErr == nil {
				fmt.Fprintf(os.Stdout, "Setup handoff: %s\n", setupHandoffOut)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	for _, name := range []string{"plan-digest", "result-out", "handoff-out", "artifact-smoke-builds"} {
		if visited[name] {
			fmt.Fprintf(os.Stderr, "error: -%s requires -apply-plan\n", name)
			os.Exit(2)
		}
	}

	// These variables seed the deployed provider. The token is retained only for
	// credential-leak validation and is never sent during prompt authoring.
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

func runOnboardDoctor(args []string) {
	fs := flag.NewFlagSet("onboard doctor", flag.ExitOnError)
	var projectDir string
	fs.StringVar(&projectDir, "project-dir", ".", "directory containing project.yaml and prompts/system.md")
	_ = fs.Parse(args)

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report := onboard.Doctor(signalCtx, onboard.DoctorOptions{ProjectDir: projectDir})
	if err := onboard.WriteDoctorReport(os.Stdout, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if report.HasFailures() {
		os.Exit(1)
	}
}

// runNotifyTest sends one test email through the consumer's configured relay.
// It exercises the same sender the fetcher uses, which lets an operator confirm
// deliverability from the deployment itself rather than waiting for an alert.
func runNotifyTest(args []string) {
	fs := flag.NewFlagSet("notify-test", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var projectDir string
	var to string
	fs.StringVar(&projectDir, "project-dir", ".", "directory containing project.yaml")
	fs.StringVar(&to, "to", "", "comma-separated recipients overriding notifications.email.to")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}

	cfg, err := project.Load(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	var recipients []string
	for _, recipient := range strings.Split(to, ",") {
		if trimmed := strings.TrimSpace(recipient); trimmed != "" {
			recipients = append(recipients, trimmed)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := notify.ProbeOptions{Config: cfg, Password: os.Getenv("EMAIL_SMTP_PASSWORD"), To: recipients}
	if err := notify.Probe(ctx, opts, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
