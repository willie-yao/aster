// Command worker runs the dashboard data pipeline continuously for the
// Kubernetes-native mode. It refreshes data on a short interval, reusing a
// cached job list, and does a full rediscovery-and-reconcile pass on a longer
// interval. It is the single writer to the output directory; use the fetcher
// CronJob instead for one-shot or GitHub Actions runs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
)

var (
	version  = "dev"
	commit   = "dev"
	imageTag = "dev"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	opts, watchInterval, reconcileInterval, err := parseOptions(args)
	if err != nil {
		return err
	}
	opts.Version = version
	opts.TraceEngine = ai.TraceEngine{Version: version, Commit: commit, ImageTag: imageTag}

	log.Printf("🌀 worker starting: out=%s watch=%s reconcile=%s", opts.OutDir, watchInterval, reconcileInterval)
	return fetcher.RunWatch(ctx, opts, watchInterval, reconcileInterval)
}

func parseOptions(args []string) (fetcher.Options, time.Duration, time.Duration, error) {
	var opts fetcher.Options
	var watchInterval, reconcileInterval time.Duration
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.StringVar(&opts.ProjectDir, "project-dir", ".", "directory containing project.yaml and prompts/system.md")
	fs.StringVar(&opts.OutDir, "out", "data", "output directory for JSON files")
	fs.IntVar(&opts.BuildsPerJob, "builds", 10, "number of recent builds to fetch per job")
	fs.IntVar(&opts.Workers, "workers", 5, "number of concurrent job fetchers")
	fs.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "per-pass fetch timeout")
	fs.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in addition to periodics")
	fs.BoolVar(&opts.EnableAI, "ai", false, "enable AI-powered failure analysis")
	fs.DurationVar(&watchInterval, "watch-interval", 5*time.Minute, "how often to refresh data reusing the cached job list")
	fs.DurationVar(&reconcileInterval, "reconcile-interval", time.Hour, "how often to rediscover jobs and run a full pass")
	analysisFlags := fetcher.BindAnalysisRuntimeFlags(fs, &opts)
	if err := fs.Parse(args); err != nil {
		return fetcher.Options{}, 0, 0, err
	}
	if err := analysisFlags.DecodePlacement(&opts); err != nil {
		return fetcher.Options{}, 0, 0, err
	}
	return opts, watchInterval, reconcileInterval, nil
}
