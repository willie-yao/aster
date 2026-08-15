package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitOpsCLI(t *testing.T) {
	dir := t.TempDir()
	writeCLITestFile(t, filepath.Join(dir, "project.yaml"), `id: sample
name: Sample
testgrid:
  dashboard: sample
storage:
  provider: gcs
  bucket: example-bucket
branding:
  title: Sample
  base_path: /
  site_url: https://example.test
  source_repo:
    owner: example
    name: sample
`)
	writeCLITestFile(t, filepath.Join(dir, "prompts", "system.md"), "diagnose from evidence\n")
	writeCLITestFile(t, filepath.Join(dir, "deploy", "values.yaml"), "mode: cron\n")
	common := []string{"--project-dir", dir, "--release", "sample", "--namespace", "sample", "--chart-version", "0.9.0"}

	render := append([]string{"gitops", "render"}, common...)
	output, exit := runCLIHelper(t, render...)
	if exit != 0 || !strings.Contains(output, "Generated 8 Flux GitOps files") {
		t.Fatalf("render exit=%d output=%q", exit, output)
	}
	check := append([]string{"gitops", "check"}, common...)
	output, exit = runCLIHelper(t, check...)
	if exit != 0 || !strings.Contains(output, "GitOps bundle is current") {
		t.Fatalf("check exit=%d output=%q", exit, output)
	}

	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{name: "invalid subcommand", args: []string{"gitops", "deploy"}, want: "gitops <render|check>"},
		{name: "unexpected argument", args: append(render, "extra"), want: "unexpected arguments"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			output, exit := runCLIHelper(t, testCase.args...)
			if exit != 2 || !strings.Contains(output, testCase.want) {
				t.Fatalf("exit=%d output=%q", exit, output)
			}
		})
	}
}

func TestGitOpsCLIHelper(t *testing.T) {
	raw := os.Getenv("ASTER_CLI_TEST_ARGS")
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	runKubernetes(args)
}

func runCLIHelper(t *testing.T, args ...string) (string, int) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGitOpsCLIHelper$")
	cmd.Env = append(os.Environ(), "ASTER_CLI_TEST_ARGS="+string(encoded))
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(output), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

func writeCLITestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
