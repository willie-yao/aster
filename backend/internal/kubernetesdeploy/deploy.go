// Package kubernetesdeploy installs a validated consumer bundle with Helm.
package kubernetesdeploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/filesystem"
	k8stools "github.com/willie-yao/aster/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/aster/backend/internal/project"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const DefaultChart = "oci://ghcr.io/willie-yao/charts/aster"

// Options select the consumer bundle and Helm release.
type Options struct {
	Action       string
	ProjectDir   string
	ValuesFile   string
	Release      string
	Namespace    string
	KubeContext  string
	Chart        string
	ChartVersion string
	DryRun       bool
}

type commandRunner interface {
	Run(context.Context, string, []string, io.Writer, io.Writer) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// Run validates the bundle and invokes Helm.
func Run(ctx context.Context, opts Options, stdout, stderr io.Writer) error {
	return run(ctx, opts, execRunner{}, stdout, stderr)
}

func run(ctx context.Context, opts Options, runner commandRunner, stdout, stderr io.Writer) error {
	resolved, skillPaths, err := validateBundle(opts)
	if err != nil {
		return err
	}

	if !resolved.DryRun {
		exists, err := releaseExists(ctx, resolved, runner)
		if err != nil {
			return err
		}
		switch {
		case resolved.Action == "install" && exists:
			return fmt.Errorf("release %q already exists in namespace %q; use kubernetes upgrade", resolved.Release, resolved.Namespace)
		case resolved.Action == "upgrade" && !exists:
			return fmt.Errorf("release %q does not exist in namespace %q; use kubernetes install", resolved.Release, resolved.Namespace)
		}
	}

	args := helmArgs(resolved, skillPaths)
	helmStdout := stdout
	if resolved.DryRun {
		helmStdout = io.Discard
	}
	if err := runner.Run(ctx, "helm", args, helmStdout, stderr); err != nil {
		return fmt.Errorf("helm %s: %w", resolved.Action, err)
	}
	if resolved.DryRun {
		fmt.Fprintf(stdout, "Validated and rendered release %q from %s with %d consumer skill files.\n", resolved.Release, resolved.ProjectDir, len(skillPaths))
	}
	return nil
}

type releaseSummary struct {
	Name string `json:"name"`
}

func releaseExists(ctx context.Context, opts Options, runner commandRunner) (bool, error) {
	args := []string{
		"list", "--all", "--filter", "^" + regexp.QuoteMeta(opts.Release) + "$",
		"--kube-context", opts.KubeContext, "--namespace", opts.Namespace, "--output", "json",
	}
	var stdout, stderr bytes.Buffer
	if err := runner.Run(ctx, "helm", args, &stdout, &stderr); err != nil {
		return false, fmt.Errorf("inspect Helm releases: %w", err)
	}
	var releases []releaseSummary
	if err := json.Unmarshal(stdout.Bytes(), &releases); err != nil {
		return false, fmt.Errorf("parse Helm release list: %w", err)
	}
	for _, release := range releases {
		if release.Name == opts.Release {
			return true, nil
		}
	}
	return false, nil
}

func validateBundle(opts Options) (Options, []string, error) {
	opts.Action = strings.TrimSpace(opts.Action)
	if opts.Action != "install" && opts.Action != "upgrade" {
		return Options{}, nil, fmt.Errorf("kubernetes action must be install or upgrade")
	}
	if strings.TrimSpace(opts.Release) == "" {
		return Options{}, nil, fmt.Errorf("--release is required")
	}
	if strings.TrimSpace(opts.Namespace) == "" {
		return Options{}, nil, fmt.Errorf("--namespace is required")
	}
	if strings.TrimSpace(opts.KubeContext) == "" {
		return Options{}, nil, fmt.Errorf("--kube-context is required; the current default context is never used implicitly")
	}

	projectDir, valuesFile, skillPaths, err := validateLocalBundle(opts.ProjectDir, opts.ValuesFile)
	if err != nil {
		return Options{}, nil, err
	}
	opts.ProjectDir, opts.ValuesFile = projectDir, valuesFile
	if strings.TrimSpace(opts.Chart) == "" {
		opts.Chart = DefaultChart
	}
	return opts, skillPaths, nil
}

func validateLocalBundle(projectDir, valuesFile string) (string, string, []string, error) {
	if strings.TrimSpace(projectDir) == "" {
		projectDir = "."
	}
	projectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve project directory: %w", err)
	}
	projectDir = filepath.Clean(projectDir)
	if strings.TrimSpace(valuesFile) == "" {
		valuesFile = filepath.Join("deploy", "values.yaml")
	}
	if !filepath.IsAbs(valuesFile) {
		valuesFile = filepath.Join(projectDir, valuesFile)
	}
	valuesFile = filepath.Clean(valuesFile)
	if err := requireRegularFile(valuesFile, "Helm values"); err != nil {
		return "", "", nil, err
	}

	cfg, _, err := project.LoadDir(projectDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("validate project bundle: %w", err)
	}
	toolSelection := cfg.AI.EffectiveAgentic().Tools
	if err := validateToolSelection(toolSelection); err != nil {
		return "", "", nil, fmt.Errorf("validate project tools: %w", err)
	}
	set, _, err := skills.LoadForTools(projectDir, toolSelection)
	if err != nil {
		return "", "", nil, fmt.Errorf("validate project skills: %w", err)
	}
	requirement := cfg.EffectiveConsumerSkills()
	if requirement.Required && !set.ConsumerBundlePresent() {
		return "", "", nil, fmt.Errorf("validate project skills: consumer skill bundle is required but not present")
	}
	if requirement.MinimumCount > 0 && set.ConsumerCount() < requirement.MinimumCount {
		return "", "", nil, fmt.Errorf("validate project skills: consumer skill count %d is below required minimum %d", set.ConsumerCount(), requirement.MinimumCount)
	}

	skillPaths, err := consumerSkillPaths(projectDir)
	if err != nil {
		return "", "", nil, err
	}
	return projectDir, valuesFile, skillPaths, nil
}

func validateToolSelection(selection []string) error {
	if len(selection) == 0 {
		return nil
	}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8stools.Register(registry)
	_, err := registry.Enable(selection)
	return err
}

func requireRegularFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("read %s %s: %w", label, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %s is not a regular file", label, path)
	}
	return nil
}

func consumerSkillPaths(projectDir string) ([]string, error) {
	skillsDir := filepath.Join(projectDir, "skills")
	info, err := os.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read consumer skills directory %s: %w", skillsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("consumer skills path %s is not a directory", skillsDir)
	}

	var paths []string
	for _, pattern := range []string{"*.yaml", "*.yml"} {
		matches, err := filepath.Glob(filepath.Join(skillsDir, pattern))
		if err != nil {
			return nil, fmt.Errorf("list consumer skills %s: %w", skillsDir, err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "project.yaml" {
			return nil, fmt.Errorf("consumer skill filename %q is reserved by the project ConfigMap", name)
		}
		if problems := k8svalidation.IsConfigMapKey(name); len(problems) > 0 {
			return nil, fmt.Errorf("consumer skill filename %q is not a valid ConfigMap key: %s", name, strings.Join(problems, ", "))
		}
		if err := requireRegularFile(path, "consumer skill"); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func helmArgs(opts Options, skillPaths []string) []string {
	var args []string
	if opts.DryRun {
		args = []string{"template", opts.Release, opts.Chart, "--namespace", opts.Namespace}
	} else {
		args = []string{"upgrade", "--install", opts.Release, opts.Chart, "--namespace", opts.Namespace, "--create-namespace", "--kube-context", opts.KubeContext}
		if opts.Action == "upgrade" {
			args = append(args, "--reset-then-reuse-values")
		}
	}
	if opts.ChartVersion != "" {
		args = append(args, "--version", opts.ChartVersion)
	}
	args = append(args,
		"--values", opts.ValuesFile,
		"--set-string", "project.existingConfigMap=",
		"--set-json", "project.skills={}",
		"--set-file", "project.config="+filepath.Join(opts.ProjectDir, "project.yaml"),
		"--set-file", "project.systemPrompt="+filepath.Join(opts.ProjectDir, "prompts", "system.md"),
	)
	for _, path := range skillPaths {
		key := strings.ReplaceAll(filepath.Base(path), ".", `\.`)
		args = append(args, "--set-file", "project.skills."+key+"="+path)
	}
	if !opts.DryRun {
		args = append(args, "--wait", "--rollback-on-failure")
	}
	return args
}
