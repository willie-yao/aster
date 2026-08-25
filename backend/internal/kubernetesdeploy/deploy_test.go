package kubernetesdeploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type recordedCommand struct {
	name string
	args []string
}

type recordingRunner struct {
	commands []recordedCommand
	releases []string
	listErr  error
	err      error
	output   string
}

func (r *recordingRunner) Run(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
	r.commands = append(r.commands, recordedCommand{name: name, args: append([]string(nil), args...)})
	if len(args) > 0 && args[0] == "list" {
		if r.listErr != nil {
			return r.listErr
		}
		releases := make([]releaseSummary, 0, len(r.releases))
		for _, release := range r.releases {
			releases = append(releases, releaseSummary{Name: release})
		}
		data, _ := json.Marshal(releases)
		_, _ = stdout.Write(data)
		return nil
	}
	if r.output != "" {
		_, _ = io.WriteString(stdout, r.output)
	}
	return r.err
}

func TestRunRequiresExplicitKubeContext(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	opts := baseOptions(dir)
	opts.KubeContext = ""

	err := run(context.Background(), opts, &recordingRunner{}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--kube-context is required") {
		t.Fatalf("error = %v, want missing kube context", err)
	}
}

func TestRunRejectsMissingAndMalformedBundleFiles(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T) string
		want    string
	}{
		{
			name: "project",
			prepare: func(t *testing.T) string {
				dir := t.TempDir()
				writeFile(t, filepath.Join(dir, "deploy", "values.yaml"), "image:\n  tag: sha-test\n")
				return dir
			},
			want: "project.yaml",
		},
		{
			name: "malformed project",
			prepare: func(t *testing.T) string {
				return writeBundle(t, minimalProject+"unknown_field: true\n", "prompt")
			},
			want: "field unknown_field not found",
		},
		{
			name: "prompt",
			prepare: func(t *testing.T) string {
				dir := writeBundle(t, minimalProject, "prompt")
				if err := os.Remove(filepath.Join(dir, "prompts", "system.md")); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want: "prompts/system.md",
		},
		{
			name: "empty prompt",
			prepare: func(t *testing.T) string {
				return writeBundle(t, minimalProject, "   \n")
			},
			want: "requires non-empty",
		},
		{
			name: "values",
			prepare: func(t *testing.T) string {
				dir := writeBundle(t, minimalProject, "prompt")
				if err := os.Remove(filepath.Join(dir, "deploy", "values.yaml")); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want: "Helm values",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.prepare(t)
			err := run(context.Background(), baseOptions(dir), &recordingRunner{}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRunRejectsUnknownToolBeforeHelm(t *testing.T) {
	projectYAML := minimalProject + "ai:\n  tools: [typo]\n"
	dir := writeBundle(t, projectYAML, "prompt")
	runner := &recordingRunner{}

	err := run(context.Background(), baseOptions(dir), runner, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `validate project tools: unknown tool or group: "typo"`) {
		t.Fatalf("error = %v, want unknown tool failure", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("invalid tool selection reached Helm: %q", runner.commands)
	}
}

func TestRunBuildsUpgradeInstallArguments(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	runner := &recordingRunner{releases: []string{"capz"}}
	opts := baseOptions(dir)
	opts.Action = "upgrade"
	opts.Chart = "./chart"
	opts.ChartVersion = "1.2.3"

	if err := run(context.Background(), opts, runner, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(runner.commands))
	}
	if got := runner.commands[0].args; len(got) == 0 || got[0] != "list" {
		t.Fatalf("first command = %q, want helm list", got)
	}
	got := runner.commands[1]
	want := []string{
		"upgrade", "--install", "capz", "./chart",
		"--namespace", "capz-dynamo", "--create-namespace", "--kube-context", "h100", "--reset-then-reuse-values",
		"--version", "1.2.3",
		"--values", filepath.Join(dir, "deploy", "values.yaml"),
		"--set-string", "project.existingConfigMap=",
		"--set-json", "project.skills={}",
		"--set-file", "project.config=" + filepath.Join(dir, "project.yaml"),
		"--set-file", "project.systemPrompt=" + filepath.Join(dir, "prompts", "system.md"),
		"--wait", "--rollback-on-failure",
	}
	if got.name != "helm" || !reflect.DeepEqual(got.args, want) {
		t.Fatalf("command = %s %q\nwant helm %q", got.name, got.args, want)
	}
}

func TestRunInstallDoesNotReuseMissingReleaseValues(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	runner := &recordingRunner{}

	if err := run(context.Background(), baseOptions(dir), runner, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	args := runner.commands[1].args
	for _, mergeFlag := range []string{"--reuse-values", "--reset-then-reuse-values"} {
		if slicesContain(args, mergeFlag) {
			t.Fatalf("fresh install unexpectedly uses %s: %q", mergeFlag, args)
		}
	}
	if !containsPair(args, "--wait", "--rollback-on-failure") {
		t.Fatalf("fresh install is not rollback guarded: %q", args)
	}
}

func TestRunEnforcesRequestedReleaseState(t *testing.T) {
	tests := []struct {
		name     string
		action   string
		releases []string
		want     string
	}{
		{name: "install existing", action: "install", releases: []string{"capz"}, want: "already exists"},
		{name: "upgrade missing", action: "upgrade", want: "does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeBundle(t, minimalProject, "prompt")
			opts := baseOptions(dir)
			opts.Action = tt.action
			runner := &recordingRunner{releases: tt.releases}
			err := run(context.Background(), opts, runner, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if len(runner.commands) != 1 || runner.commands[0].args[0] != "list" {
				t.Fatalf("release-state failure reached Helm write: %q", runner.commands)
			}
		})
	}
}

func TestRunDryRunRendersLocallyWithoutPrintingManifest(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	runner := &recordingRunner{output: "manifest with inline values"}
	opts := baseOptions(dir)
	opts.DryRun = true
	var stdout bytes.Buffer

	if err := run(context.Background(), opts, runner, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(runner.commands))
	}
	args := runner.commands[0].args
	if len(args) < 3 || args[0] != "template" {
		t.Fatalf("dry-run args = %q, want helm template", args)
	}
	if slicesContain(args, "--kube-context") || slicesContain(args, "upgrade") {
		t.Fatalf("dry-run unexpectedly uses the cluster: %q", args)
	}
	if strings.Contains(stdout.String(), "manifest with inline values") {
		t.Fatalf("dry-run printed rendered manifest: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Validated and rendered release \"capz\"") {
		t.Fatalf("dry-run summary = %q", stdout.String())
	}
}

func TestRunIncludesValidatedConsumerSkillsAndClearsStaleValues(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	writeFile(t, filepath.Join(dir, "skills", "zeta.yml"), "id: zeta\ntriggers: [failure]\n")
	writeFile(t, filepath.Join(dir, "skills", "alpha.recipe.yaml"), "id: alpha\ntriggers: [failure]\n")
	runner := &recordingRunner{}

	if err := run(context.Background(), baseOptions(dir), runner, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	args := runner.commands[1].args
	want := []string{
		"project.skills.alpha\\.recipe\\.yaml=" + filepath.Join(dir, "skills", "alpha.recipe.yaml"),
		"project.skills.zeta\\.yml=" + filepath.Join(dir, "skills", "zeta.yml"),
	}
	var got []string
	for i, arg := range args {
		if arg == "--set-file" && i+1 < len(args) && strings.HasPrefix(args[i+1], "project.skills.") {
			got = append(got, args[i+1])
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skill args = %q, want %q", got, want)
	}
	if !containsPair(args, "--set-json", "project.skills={}") {
		t.Fatalf("arguments do not clear stale project.skills: %q", args)
	}
}

func TestRunRejectsReservedSkillFilename(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	writeFile(t, filepath.Join(dir, "skills", "project.yaml"), "id: reserved-project\ntriggers: [failure]\n")
	err := run(context.Background(), baseOptions(dir), &recordingRunner{}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is reserved by the project ConfigMap") {
		t.Fatalf("error = %v, want reserved filename failure", err)
	}
}

func TestRunRejectsSkillFilenameThatCannotBeMounted(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	writeFile(t, filepath.Join(dir, "skills", "bad name.yaml"), "id: bad-name\ntriggers: [failure]\n")
	err := run(context.Background(), baseOptions(dir), &recordingRunner{}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a valid ConfigMap key") {
		t.Fatalf("error = %v, want ConfigMap key failure", err)
	}
}

func TestRunRejectsMalformedAndMissingRequiredSkills(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		dir := writeBundle(t, minimalProject, "prompt")
		writeFile(t, filepath.Join(dir, "skills", "bad.yaml"), "id: [")
		err := run(context.Background(), baseOptions(dir), &recordingRunner{}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "validate project skills") {
			t.Fatalf("error = %v, want skill validation failure", err)
		}
	})

	t.Run("required", func(t *testing.T) {
		projectYAML := minimalProject + "ai:\n  consumer_skills:\n    required: true\n"
		dir := writeBundle(t, projectYAML, "prompt")
		err := run(context.Background(), baseOptions(dir), &recordingRunner{}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "consumer skill bundle is required but not present") {
			t.Fatalf("error = %v, want required skill failure", err)
		}
	})
}

func TestRunReturnsHelmFailure(t *testing.T) {
	dir := writeBundle(t, minimalProject, "prompt")
	runner := &recordingRunner{err: fmt.Errorf("exit status 1")}
	err := run(context.Background(), baseOptions(dir), runner, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "helm install") {
		t.Fatalf("error = %v, want Helm context", err)
	}
}

func baseOptions(dir string) Options {
	return Options{
		Action:      "install",
		ProjectDir:  dir,
		ValuesFile:  filepath.Join("deploy", "values.yaml"),
		Release:     "capz",
		Namespace:   "capz-dynamo",
		KubeContext: "h100",
	}
}

func writeBundle(t *testing.T, projectYAML, prompt string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "project.yaml"), projectYAML)
	writeFile(t, filepath.Join(dir, "prompts", "system.md"), prompt)
	writeFile(t, filepath.Join(dir, "deploy", "values.yaml"), "image:\n  tag: sha-test\n")
	return dir
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsPair(values []string, first, second string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == first && values[i+1] == second {
			return true
		}
	}
	return false
}

const minimalProject = `id: test
name: Test
discovery:
  testgrid_dashboard: test
storage:
  provider: local
  base: /tmp
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo:
    owner: example
    name: example
`
