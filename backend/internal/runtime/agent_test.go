package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	fullRepo := RepoRef{Owner: "o", Name: "n", Ref: "r"}
	if _, err := r.Generate(context.Background(), GenerateSpec{Instruction: "x", Repo: fullRepo, Model: "m", NativeModel: "p/m"}); err == nil {
		t.Error("expected model mode conflict")
	}
	if _, err := r.Generate(context.Background(), GenerateSpec{Instruction: "x", Repo: fullRepo, UseAmbientAuth: true}); err == nil {
		t.Error("expected ambient auth without native model to fail")
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
		Model:        "moonshotai/Kimi-K2",
		Endpoint:     "https://host/v1/chat/completions",
		Token:        "tok",
		MaxTurns:     30,
		AllowBash:    true,
		ExtraHeaders: map[string]string{"Copilot-Integration-Id": "copilot-developer-cli"},
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
				BaseURL string            `json:"baseURL"`
				APIKey  string            `json:"apiKey"`
				Headers map[string]string `json:"headers"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
		Permission map[string]string `json:"permission"`
		Agent      map[string]struct {
			Steps int `json:"steps"`
		} `json:"agent"`
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
	if eng.Options.Headers["Copilot-Integration-Id"] != "copilot-developer-cli" {
		t.Errorf("headers = %v", eng.Options.Headers)
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
	if cfg.Agent["build"].Steps != 30 {
		t.Errorf("build agent steps = %d, want 30", cfg.Agent["build"].Steps)
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
		Agent      map[string]struct {
			Steps int `json:"steps"`
		} `json:"agent"`
	}
	_ = json.Unmarshal(b, &cfg)
	if cfg.Permission["bash"] != "deny" {
		t.Errorf("bash permission = %q, want deny (AllowBash=false)", cfg.Permission["bash"])
	}
	if _, ok := cfg.Agent["build"]; ok {
		t.Errorf("build agent steps should use the OpenCode default when MaxTurns is zero: %v", cfg.Agent)
	}
}

func TestOpencodeCmdPinsRuntimeConfig(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "opencode.json"), []byte(`{"default_agent":"other","agent":{"build":{"steps":999}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(opencodeConfigEnv, "/tmp/ambient.json")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"default_agent":"other"}`)
	t.Setenv(opencodeDisableProjectEnv, "false")

	cmd, err := opencodeCmd("opencode")(context.Background(), GenerateSpec{
		Instruction: "fix it", Endpoint: "https://host/v1", Token: "tok", MaxTurns: 30,
	}, workdir, home)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Args, "--agent") {
		t.Fatalf("args missing --agent: %v", cmd.Args)
	}
	for i, arg := range cmd.Args {
		if arg == "--agent" && (i+1 >= len(cmd.Args) || cmd.Args[i+1] != "build") {
			t.Fatalf("agent args = %v, want build", cmd.Args)
		}
	}
	env := map[string]string{}
	for _, entry := range cmd.Env {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			env[name] = value
		}
	}
	if got := env[opencodeConfigEnv]; got != filepath.Join(home, ".config", "opencode", "opencode.json") {
		t.Fatalf("%s = %q", opencodeConfigEnv, got)
	}
	for _, name := range []string{opencodeDisableProjectEnv, opencodeDisableUpdateEnv, opencodeDisableSkillsEnv} {
		if env[name] != "true" {
			t.Errorf("%s = %q, want true", name, env[name])
		}
	}
	if _, ok := env["OPENCODE_CONFIG_CONTENT"]; ok {
		t.Error("ambient OPENCODE_CONFIG_CONTENT was preserved")
	}
}

func TestOpenAIBaseResponses(t *testing.T) {
	if got := openAIBase("https://api.example.test/v1/responses"); got != "https://api.example.test/v1" {
		t.Fatalf("openAIBase = %q", got)
	}
}

func TestWriteOpencodeSkills(t *testing.T) {
	home := t.TempDir()
	if err := writeOpencodeSkills(home, map[string]string{"system-prompt-generation": "---\nname: system-prompt-generation\n---\nbody\n"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "skills", "system-prompt-generation", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "system-prompt-generation") {
		t.Fatalf("skill contents = %q", got)
	}
	for _, name := range []string{"", "../escape", "Uppercase", "bad_name"} {
		if err := writeOpencodeSkills(home, map[string]string{name: "body"}); err == nil {
			t.Errorf("expected invalid skill name %q to fail", name)
		}
	}
	if err := writeOpencodeSkills(home, map[string]string{"empty": " \n"}); err == nil {
		t.Error("expected empty skill to fail")
	}
}

func TestWriteOpencodeAuthFiltersProvider(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	sourceDir := filepath.Join(userHome, ".local", "share", "opencode")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth","access":"secret"},"other":{"type":"api","key":"other-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := writeOpencodeAuth(home, "github-copilot/claude-sonnet-4.6"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["github-copilot"] == nil {
		t.Fatalf("auth providers = %v", got)
	}
	if strings.Contains(string(raw), "other-secret") {
		t.Fatal("unselected credential copied")
	}
}

func TestWriteOpencodeAuthUsesXDGDataHome(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	sourceDir := filepath.Join(dataHome, "opencode")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth","access":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOpencodeAuth(t.TempDir(), "github-copilot/model"); err != nil {
		t.Fatal(err)
	}
}

func TestOpencodeCmdUsesNativeModel(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	sourceDir := filepath.Join(userHome, ".local", "share", "opencode")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth","access":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	home, workdir := t.TempDir(), t.TempDir()
	cmd, err := opencodeCmd("opencode")(context.Background(), GenerateSpec{
		Instruction: "author prompt", NativeModel: "github-copilot/claude-sonnet-4.6", UseAmbientAuth: true,
	}, workdir, home)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Args, "github-copilot/claude-sonnet-4.6") {
		t.Fatalf("args = %v", cmd.Args)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"engine"`) {
		t.Fatalf("native config retained custom provider: %s", raw)
	}
}
