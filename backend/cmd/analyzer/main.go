// Command analyzer runs the dashboard-owned policy for one failure request.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

type envGetter func(string) string

type analyzerRuntime struct {
	analyzer   ai.FailureAnalyzer
	httpClient *http.Client
	snapshot   func(ai.FailureAnalysisRequest) (analysisruntime.ContainerAnalysisState, error)
}

type runtimeFactory func(context.Context, commandOptions, envGetter) (*analyzerRuntime, error)

type commandOptions struct {
	projectDir string
	dataDir    string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr, loadRuntime); err != nil {
		writeAnalyzerError(os.Stderr, err)
		os.Exit(1)
	}
}

func writeAnalyzerError(w io.Writer, err error) {
	fmt.Fprintf(w, "analyzer: %s\n", redact.URLs(err.Error()))
}

type redactingWriter struct {
	w io.Writer
}

func (w redactingWriter) Write(data []byte) (int, error) {
	sanitized := []byte(redact.URLs(string(data)))
	if _, err := w.w.Write(sanitized); err != nil {
		return 0, err
	}
	return len(data), nil
}

func run(ctx context.Context, args []string, getenv envGetter, stdout, stderr io.Writer, factory runtimeFactory) error {
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(redactingWriter{w: stderr})
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	}()

	var opts commandOptions
	flags := flag.NewFlagSet("analyzer", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.dataDir, "data-dir", "/tmp/prow-ai-analyzer", "private cache and trace directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}

	bundle, err := analysisruntime.DecodeProjectBundle([]byte(getenv(analysisruntime.ProjectBundleEnv)))
	if err != nil {
		return err
	}
	if err := analysisruntime.VerifyProjectBundleDigest(bundle, getenv(analysisruntime.ProjectBundleDigestEnv)); err != nil {
		return err
	}
	if err := analysisruntime.VerifyProjectBundleContract(bundle); err != nil {
		return err
	}
	stateKey, err := analysisruntime.ParseContainerStateKey(getenv(analysisruntime.ContainerStateKeyEnv))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.dataDir, 0o700); err != nil {
		return fmt.Errorf("create private data directory: %w", err)
	}
	if err := analysisruntime.RemoveContainerLocalState(opts.dataDir); err != nil {
		return fmt.Errorf("clear private container state: %w", err)
	}
	if err := analysisruntime.RestoreContainerCache(opts.dataDir, bundle.Request, bundle.CacheSeed); err != nil {
		return fmt.Errorf("restore private container cache: %w", err)
	}
	projectDir, cleanup, err := analysisruntime.MaterializeProjectBundle(bundle)
	if err != nil {
		return err
	}
	defer cleanup()
	opts.projectDir = projectDir

	runtime, err := factory(ctx, opts, getenv)
	if err != nil {
		return err
	}
	log.Printf("starting failure analysis bundle=%s", bundle.Digest[:12])
	result, analyzeErr := runtime.analyzer.AnalyzeFailure(ctx, runtime.httpClient, bundle.Request)
	state, stateErr := runtime.snapshot(bundle.Request)
	if analyzeErr != nil {
		return errors.Join(fmt.Errorf("AnalyzeFailure: %w", analyzeErr), stateErr)
	}
	if stateErr != nil {
		return fmt.Errorf("snapshot private analysis state: %w", stateErr)
	}
	identity := analysisruntime.NewContainerStateIdentity(getenv(analysisruntime.ContainerTaskNamespaceEnv), getenv(analysisruntime.ContainerTaskNameEnv), bundle.Request)
	if err := analysisruntime.WriteEncryptedContainerAnalysisState(stdout, state, stateKey, identity); err != nil {
		return err
	}
	if err := analysisruntime.WriteFailureAnalysisResult(stdout, result); err != nil {
		return err
	}
	return nil
}

func loadRuntime(ctx context.Context, opts commandOptions, getenv envGetter) (*analyzerRuntime, error) {
	if err := os.MkdirAll(opts.dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create private data directory: %w", err)
	}
	cfg, err := project.Load(filepath.Join(opts.projectDir, "project.yaml"))
	if err != nil {
		return nil, fmt.Errorf("loading project config: %w", err)
	}
	analysisProject, err := analysisruntime.LoadProject(opts.projectDir, cfg, analysisruntime.ProviderFallbacks{
		API: getenv("AI_API"), Endpoint: getenv("AI_ENDPOINT"), Model: getenv("AI_MODEL"),
	})
	if err != nil {
		return nil, err
	}
	token := getenv("AI_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("AI_TOKEN is required")
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	backend, err := storage.New(cfg.StorageConfig(), httpClient)
	if err != nil {
		return nil, fmt.Errorf("configuring storage: %w", err)
	}
	runtime, err := analysisruntime.New(ctx, analysisruntime.Options{
		Token: token, DataDir: opts.dataDir, Project: analysisProject,
	})
	if err != nil {
		return nil, err
	}
	traceStore := ai.NewTraceStore()
	service, err := runtime.NewService(analysisruntime.ServiceOptions{
		Backend:    backend,
		TraceStore: traceStore,
		TraceMetadata: ai.TraceMetadata{
			Backend: "orka", TaskNamespace: getenv(analysisruntime.ContainerTaskNamespaceEnv),
			TaskName: getenv(analysisruntime.ContainerTaskNameEnv), ContractHash: getenv(analysisruntime.ContainerContractVersionEnv),
		},
	})
	if err != nil {
		return nil, err
	}
	runtime.LogConfiguration()
	tracePath := filepath.Join(opts.dataDir, output.AITraceFilename)
	return &analyzerRuntime{
		analyzer:   service,
		httpClient: httpClient,
		snapshot: func(request ai.FailureAnalysisRequest) (analysisruntime.ContainerAnalysisState, error) {
			if err := errors.Join(runtime.SaveCache(), traceStore.Save(tracePath)); err != nil {
				return analysisruntime.ContainerAnalysisState{}, err
			}
			identity := analysisruntime.NewContainerStateIdentity(getenv(analysisruntime.ContainerTaskNamespaceEnv), getenv(analysisruntime.ContainerTaskNameEnv), request)
			return analysisruntime.SnapshotContainerAnalysisState(runtime.Client.Cache(), traceStore, request, identity)
		},
	}, nil
}
