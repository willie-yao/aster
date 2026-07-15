package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeAgent returns a buildCmd that runs a shell script in the workspace,
// standing in for a coding CLI that edits files.
func fakeAgent(script string) func(ctx context.Context, spec GenerateSpec, workdir, home string) (*exec.Cmd, error) {
	return func(ctx context.Context, _ GenerateSpec, workdir, _ string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		cmd.Dir = workdir
		return cmd, nil
	}
}

func TestLocalAgent_ReturnsChangedFiles(t *testing.T) {
	repo := initRepo(t)
	r := &LocalAgentRuntime{
		Bin:      "true", // present on PATH; the fake buildCmd is what runs
		buildCmd: fakeAgent(`printf 'fixed\n' > fix.txt && printf 'more\n' >> orig.txt`),
	}
	got, err := r.Generate(context.Background(), GenerateSpec{
		Repo:        RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Instruction: "fix the thing",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Files["fix.txt"] != "fixed\n" {
		t.Errorf("fix.txt = %q, want %q", got.Files["fix.txt"], "fixed\n")
	}
	if got.Files["orig.txt"] != "base\nmore\n" {
		t.Errorf("orig.txt = %q, want %q", got.Files["orig.txt"], "base\nmore\n")
	}
	if got.Diff == "" {
		t.Error("expected a non-empty diff")
	}
}

func TestLocalAgent_NoChangeEmpty(t *testing.T) {
	repo := initRepo(t)
	r := &LocalAgentRuntime{Bin: "true", buildCmd: fakeAgent(`true`)}
	got, err := r.Generate(context.Background(), GenerateSpec{
		Repo:        RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Instruction: "do nothing",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(got.Files) != 0 {
		t.Errorf("expected no changed files, got %v", got.Files)
	}
}

func TestLocalAgent_NonzeroExitStillCollectsEdits(t *testing.T) {
	repo := initRepo(t)
	// The CLI edits a file then exits non-zero; the edit must still be collected.
	r := &LocalAgentRuntime{Bin: "true", buildCmd: fakeAgent(`printf 'x\n' > fix.txt; exit 3`)}
	got, err := r.Generate(context.Background(), GenerateSpec{
		Repo:        RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo},
		Instruction: "fix and fail",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Files["fix.txt"] != "x\n" {
		t.Errorf("fix.txt = %q, want %q", got.Files["fix.txt"], "x\n")
	}
}

func TestLocalAgent_Validation(t *testing.T) {
	r := &LocalAgentRuntime{Bin: "true", buildCmd: fakeAgent(`true`)}
	if _, err := r.Generate(context.Background(), GenerateSpec{Instruction: ""}); err == nil {
		t.Error("expected error for empty instruction")
	}
	if _, err := r.Generate(context.Background(), GenerateSpec{Instruction: "x", Repo: RepoRef{Owner: "o"}}); err == nil {
		t.Error("expected error for missing repo fields")
	}
}

func TestOpenAIBase(t *testing.T) {
	cases := map[string]string{
		"https://api.githubcopilot.com/chat/completions": "https://api.githubcopilot.com",
		"https://host/v1/chat/completions":               "https://host/v1",
		"https://host/v1/":                               "https://host/v1",
		"https://host/v1":                                "https://host/v1",
	}
	for in, want := range cases {
		if got := openAIBase(in); got != want {
			t.Errorf("openAIBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteOpencodeConfig(t *testing.T) {
	home := t.TempDir()
	spec := GenerateSpec{
		Model:     "moonshotai/Kimi-K2",
		Endpoint:  "https://host/v1/chat/completions",
		Token:     "tok",
		AllowBash: true,
	}
	if err := writeOpencodeConfig(home, spec); err != nil {
		t.Fatalf("writeOpencodeConfig: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
		Permission map[string]string `json:"permission"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	eng, ok := cfg.Provider["engine"]
	if !ok {
		t.Fatal("provider engine missing")
	}
	if eng.Options.BaseURL != "https://host/v1" {
		t.Errorf("baseURL = %q, want https://host/v1", eng.Options.BaseURL)
	}
	if eng.Options.APIKey != "tok" {
		t.Errorf("apiKey = %q, want tok", eng.Options.APIKey)
	}
	if _, ok := eng.Models["moonshotai/Kimi-K2"]; !ok {
		t.Errorf("model not registered: %v", eng.Models)
	}
	if cfg.Permission["edit"] != "allow" {
		t.Errorf("edit permission = %q, want allow", cfg.Permission["edit"])
	}
	if cfg.Permission["bash"] != "allow" {
		t.Errorf("bash permission = %q, want allow (AllowBash=true)", cfg.Permission["bash"])
	}
}

func TestWriteOpencodeConfig_BashDeniedByDefault(t *testing.T) {
	home := t.TempDir()
	if err := writeOpencodeConfig(home, GenerateSpec{Endpoint: "https://h/v1", Token: "t"}); err != nil {
		t.Fatalf("writeOpencodeConfig: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	var cfg struct {
		Permission map[string]string `json:"permission"`
	}
	_ = json.Unmarshal(b, &cfg)
	if cfg.Permission["bash"] != "deny" {
		t.Errorf("bash permission = %q, want deny (AllowBash=false)", cfg.Permission["bash"])
	}
}
