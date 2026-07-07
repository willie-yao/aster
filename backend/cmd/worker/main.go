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

	collectorgeneric "github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors/generic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
)

var version = "dev"

func main() {
	var opts fetcher.Options
	var watchInterval, reconcileInterval time.Duration
	flag.StringVar(&opts.ProjectDir, "project-dir", ".", "directory containing project.yaml and prompts/system.md")
	flag.StringVar(&opts.OutDir, "out", "data", "output directory for JSON files")
	flag.IntVar(&opts.BuildsPerJob, "builds", 10, "number of recent builds to fetch per job")
	flag.IntVar(&opts.Workers, "workers", 5, "number of concurrent job fetchers")
	flag.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "per-pass fetch timeout")
	flag.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in addition to periodics")
	flag.BoolVar(&opts.EnableAI, "ai", false, "enable AI-powered failure analysis")
	flag.DurationVar(&watchInterval, "watch-interval", 5*time.Minute, "how often to refresh data reusing the cached job list")
	flag.DurationVar(&reconcileInterval, "reconcile-interval", time.Hour, "how often to rediscover jobs and run a full pass")
	flag.Parse()

	opts.Version = version
	opts.Collectors = fetcher.NewCollectorRegistry()
	opts.Collectors.Register("generic", collectorgeneric.Factory)

	// Cancel the watch loop on SIGINT/SIGTERM so K8s rollouts drain cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("🌀 worker starting: out=%s watch=%s reconcile=%s", opts.OutDir, watchInterval, reconcileInterval)
	if err := fetcher.RunWatch(ctx, opts, watchInterval, reconcileInterval); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
