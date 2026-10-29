package onboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/project"
	"gopkg.in/yaml.v3"
)

func TestReusableDeployContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".github", "workflows", "reusable-deploy.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reusable deploy workflow: %v", err)
	}
	workflow := string(data)
	for _, want := range []string{
		"group: prow-ai-dashboard-${{ github.repository }}-${{ inputs.project_dir }}",
		"cancel-in-progress: false",
		"ai-reasoning-effort:",
		"AI_REASONING_EFFORT: ${{ inputs.ai-reasoning-effort }}",
		"ai-cache-generation:",
		"AI_CACHE_GENERATION: ${{ inputs.ai-cache-generation }}",
		"WORKFLOW_REPOSITORY: ${{ job.workflow_repository }}",
		"WORKFLOW_REF: ${{ job.workflow_ref }}",
		"WORKFLOW_SHA: ${{ job.workflow_sha }}",
		"repository: ${{ steps.engine-identity.outputs.repository }}",
		"ref: ${{ steps.engine-identity.outputs.sha }}",
		"ACTUAL_SHA=\"$(git rev-parse HEAD)\"",
		"engine/frontend/public/data/provenance.json",
		"reusable_workflow_sha: process.env.WORKFLOW_SHA",
		"engine_commit: process.env.ENGINE_COMMIT",
		"manifest?.discovery?.resolved_test_infra_revision",
		"provenance.test_infra_revision = revision",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("reusable deploy workflow missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"github.job_workflow_sha",
		"repository: willie-yao/aster\n          # Build the engine code",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("reusable deploy workflow contains stale contract %q", forbidden)
		}
	}
}

func TestReusableDeployBasePath(t *testing.T) {
	root := onboardingRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "reusable-deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID  string            `yaml:"id"`
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["build-and-deploy"].Steps {
		if step.ID == "cfg" {
			script = step.Run
			if step.Env["PROJECT_DIR"] != "${{ inputs.project_dir }}" {
				t.Fatal("cfg step must pass project_dir through the environment")
			}
		}
	}
	if script == "" {
		t.Fatal("missing cfg workflow step")
	}

	const prefix = "id: test\nname: Test\ndiscovery:\n  testgrid_dashboard: test\nstorage:\n  bucket: test\nbranding:\n"
	const suffix = "  site_url: https://example.com\n  source_repo:\n    owner: example\n    name: test\n"
	tests := []struct {
		name      string
		value     string
		config    string
		want      string
		wantError string
		missing   bool
	}{
		{name: "bare root", value: "/", want: "/"},
		{name: "bare subpath", value: "/dashboard", want: "/dashboard/"},
		{name: "existing slash", value: "/dashboard/", want: "/dashboard/"},
		{name: "bare comment", value: "/dashboard # Pages", want: "/dashboard/"},
		{name: "bare whitespace", value: "/dashboard \t ", want: "/dashboard/"},
		{name: "bare slash whitespace", value: "/dashboard/ \t# Pages", want: "/dashboard/"},
		{name: "single quoted", value: "'/dashboard'", want: "/dashboard/"},
		{name: "double quoted", value: `"/dashboard"`, want: "/dashboard/"},
		{name: "single quoted comment", value: "'/dashboard/' \t# Pages", want: "/dashboard/"},
		{name: "double quoted comment", value: "\"/dashboard\" \t# Pages", want: "/dashboard/"},
		{name: "quoted root", value: `"/" # root`, want: "/"},
		{name: "bare hash", value: "/dash#board # Pages", want: "/dash#board/"},
		{name: "single quoted hash", value: "'/dash # board' # Pages", want: "/dash # board/"},
		{name: "double quoted hash", value: `"/dash # board" # Pages`, want: "/dash # board/"},
		{name: "bare quotes", value: `/dash'board" # Pages`, want: `/dash'board"/`},
		{name: "single quoted double quotes", value: `'/dash"board"' # Pages`, want: `/dash"board"/`},
		{name: "double quoted apostrophes", value: `"/dash'board'" # Pages`, want: "/dash'board'/"},
		{name: "single quoted backslash", value: `'/dash\board' # Pages`, want: `/dash\board/`},
		{name: "single quoted apostrophe", value: `'/dash''board' # Pages`, want: "/dash'board/"},
		{name: "double quoted escapes", value: `"/dash\"board\\end" # Pages`, want: "/dash\"board\\end/"},
		{name: "double quoted unicode escape", value: `"/dash\u0062oard" # Pages`, want: "/dashboard/"},
		{name: "double quoted hex escape", value: `"/dash\x62oard" # Pages`, want: "/dashboard/"},
		{name: "quoted whitespace", value: "' /dashboard ' # Pages", want: " /dashboard /"},
		{name: "literal shell syntax", value: `"/$(touch injected)" # Pages`, want: "/$(touch injected)/"},
		{name: "commented branding", config: "branding: # Pages\n  base_path: '/dashboard' # path\n", want: "/dashboard/"},
		{name: "nested field", config: "branding:\n  source_repo:\n    base_path: /wrong\n  base_path: /dashboard\n", want: "/dashboard/"},
		{name: "missing config", missing: true, wantError: "::error::project.yaml not found at "},
		{name: "missing branding", config: "other:\n  base_path: /wrong\n", wantError: "::error::branding.base_path missing in "},
		{name: "missing path", config: "branding:\n  title: Test\nother:\n  base_path: /wrong\n", wantError: "::error::branding.base_path missing in "},
		{name: "nested branding", config: "other:\n  branding:\n    base_path: /wrong\n", wantError: "::error::branding.base_path missing in "},
		{name: "missing nested path", config: "branding:\n  source_repo:\n    base_path: /wrong\nother:\n  base_path: /wrong\n", wantError: "::error::branding.base_path missing in "},
		{name: "empty path", value: `"" # empty`, wantError: "::error::branding.base_path missing in "},
		{name: "null path", value: "null # empty", wantError: "::error::branding.base_path missing in "},
		{name: "blank path", value: " # empty", wantError: "::error::branding.base_path missing in "},
		{name: "whitespace path", value: "' \t ' # empty", wantError: "::error::branding.base_path missing in "},
		{name: "unclosed quote", value: `"/dashboard # Pages`, wantError: "::error::Cannot read branding.base_path in "},
		{name: "invalid escape", value: `"/dash\qboard"`, wantError: "::error::Cannot read branding.base_path in "},
		{name: "line break escape", value: `"/dash\nboard"`, wantError: "::error::Cannot read branding.base_path in "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			consumer := filepath.Join(workspace, "consumer")
			if err := os.Mkdir(consumer, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(root, filepath.Join(workspace, "engine")); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(consumer, "project.yaml")
			if !tt.missing {
				config := tt.config
				if config == "" {
					config = prefix + "  base_path: " + tt.value + "\n" + suffix
				}
				if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				if tt.config == "" && tt.wantError == "" {
					cfg, err := project.Load(configPath)
					if err != nil {
						t.Fatalf("fixture rejected by project validator: %v", err)
					}
					want := cfg.Branding.BasePath
					if !strings.HasSuffix(want, "/") {
						want += "/"
					}
					if want != tt.want {
						t.Fatalf("fixture decodes to %q, want %q", want, tt.want)
					}
				}
			}
			outputPath := filepath.Join(workspace, "output")
			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = workspace
			cmd.Env = append(os.Environ(), "GITHUB_WORKSPACE="+workspace, "GITHUB_OUTPUT="+outputPath, "PROJECT_DIR=.")
			logs, err := cmd.CombinedOutput()
			if tt.wantError != "" {
				if err == nil || !strings.Contains(string(logs), tt.wantError) {
					t.Fatalf("got error %v and logs %s, want %q", err, logs, tt.wantError)
				}
				if output, _ := os.ReadFile(outputPath); len(output) != 0 {
					t.Fatalf("failed resolution wrote outputs: %s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("workflow step: %v\n%s", err, logs)
			}
			output, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			want := "base_path=" + tt.want + "\nproject_dir_abs=" + consumer + "/.\nslug=root\n"
			if string(output) != want {
				t.Fatalf("workflow outputs = %q, want %q", output, want)
			}
			if _, err := os.Stat(filepath.Join(workspace, "injected")); !os.IsNotExist(err) {
				t.Fatalf("config content was executed: %v", err)
			}
		})
	}
}
