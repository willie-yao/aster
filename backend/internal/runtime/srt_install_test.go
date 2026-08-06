package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSRTInstallerContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	installerPath := filepath.Join(root, "hack", "install-srt.sh")
	installer, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("read srt installer: %v", err)
	}
	text := string(installer)
	for _, want := range []string{
		`srt_version="` + SRTVersion + `"`,
		`srt_commit="44ab607c46f20381aeaf3e22ca0e0151d4c6b29c"`,
		`source_sha256="5fc9680a0431bb9172eba591f5289756b8d57a5353941b139df4106c000979f0"`,
		`installer_schema="2"`,
		"npm ci --ignore-scripts",
		"npm ci ${mode} dependency attempt ${attempt} failed verification",
		"node_modules/.bin/tsc",
		"npm run build",
		"run_npm_ci runtime --omit=dev",
		"cp -R \"${source_dir}/node_modules/.\" \"${stage}/node_modules/\"",
		`node "${script_dir}/build-srt-seccomp.mjs" "${source_dir}"`,
		`installed_provenance="${destination}/INSTALL_PROVENANCE"`,
		`installer_schema=${installer_schema}`,
		`source_commit=${srt_commit}`,
		`source_sha256=${source_sha256}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("srt installer missing %q", want)
		}
	}
	if strings.Contains(text, "@anthropic-ai/sandbox-runtime@") {
		t.Fatal("srt installer must not depend on the unpublished registry package")
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n", installerPath)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v: %s", err, output)
		}
	}
	seccompBuilder := filepath.Join(root, "hack", "build-srt-seccomp.mjs")
	if node, err := exec.LookPath("node"); err == nil {
		cmd := exec.Command(node, "--check", seccompBuilder)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node --check: %v: %s", err, output)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerText := string(dockerfile)
	if !strings.Contains(dockerText, "install-srt /usr/local/share/prow-ai-dashboard/srt") {
		t.Fatal("fixer image does not use the verified srt installer")
	}
	if strings.Contains(dockerText, `npm install -g "opencode-ai@${OPENCODE_VERSION}" "@anthropic-ai/sandbox-runtime@`) {
		t.Fatal("fixer image still installs srt from the package registry")
	}

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "srt-integration.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	workflowText := string(workflow)
	for _, want := range []string{
		"name: SRT integration",
		"runs-on: ubuntu-22.04",
		"workflow_dispatch:",
		`cron: "17 9 * * 1"`,
		`"backend/internal/runtime/srt_*.go"`,
		`./hack/install-srt.sh "$RUNNER_TEMP/srt-0.0.70"`,
		`"$SRT_BIN" --help`,
		"installer_schema=2",
		"TestSRTSandbox(Hostile|Cancellation)Integration",
	} {
		if !strings.Contains(workflowText, want) {
			t.Errorf("CI workflow missing %q", want)
		}
	}
	ciWorkflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	if strings.Contains(string(ciWorkflow), "srt-installer:") || strings.Contains(string(ciWorkflow), "TestSRTSandboxHostileIntegration") {
		t.Fatal("host-dependent srt integration is still part of required per-PR CI")
	}
}
