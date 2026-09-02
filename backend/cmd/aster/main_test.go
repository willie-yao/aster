package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitOpsCLI(t *testing.T) {
	dir := t.TempDir()
	writeCLITestFile(t, filepath.Join(dir, "project.yaml"), `id: sample
name: Sample
discovery:
  testgrid_dashboard: sample
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
	runKubernetes(t.Context(), args)
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

func TestAsterRejectsRemovedPresubmitOverride(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAsterCommandHelper$")
	cmd.Env = append(os.Environ(), "ASTER_COMMAND_TEST=removed-presubmit")
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "flag provided but not defined: -include-presubmits") {
		t.Fatalf("exit=%v output=%q", err, output)
	}
}

func TestAsterRejectsRemovedNoPromptAlias(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAsterCommandHelper$")
	cmd.Env = append(os.Environ(), "ASTER_COMMAND_TEST=removed-no-prompt")
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "flag provided but not defined: -no-prompt") {
		t.Fatalf("exit=%v output=%q", err, output)
	}
}

func TestSignalRootContextCancelsThenRestoresDefault(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAsterCommandHelper$")
	cmd.Env = append(os.Environ(), "ASTER_COMMAND_TEST=signals")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("ready output = %q, err=%v", scanner.Text(), scanner.Err())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "cancelled" {
		t.Fatalf("cancel output = %q, err=%v", scanner.Text(), scanner.Err())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("second signal did not terminate process: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second signal did not restore immediate process termination")
	}
}

func TestAsterCommandHelper(t *testing.T) {
	switch os.Getenv("ASTER_COMMAND_TEST") {
	case "removed-presubmit":
		flag.CommandLine = flag.NewFlagSet("aster", flag.ExitOnError)
		os.Args = []string{"aster", "-include-presubmits"}
		main()
	case "removed-no-prompt":
		flag.CommandLine = flag.NewFlagSet("aster", flag.ExitOnError)
		os.Args = []string{"aster", "onboard", "-no-prompt"}
		main()
	case "signals":
		ctx, stop := signalRootContext()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("cancelled")
		time.Sleep(30 * time.Second)
	}
}
