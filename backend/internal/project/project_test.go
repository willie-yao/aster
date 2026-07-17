package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
id: capz
name: "Cluster API Provider Azure"
short_name: "CAPZ"
testgrid:
  dashboard: "sig-cluster-lifecycle-cluster-api-provider-azure"
storage:
  provider: "gcs"
  bucket: "kubernetes-ci-logs"
branding:
  title: "CAPZ Prow Dashboard"
  base_path: "/capz-prow-dashboard"
  site_url: "https://willie-yao.github.io/capz-prow-dashboard"
  source_repo:
    owner: "kubernetes-sigs"
    name: "cluster-api-provider-azure"
`

func TestParseValid(t *testing.T) {
	c, err := parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.ID != "capz" {
		t.Errorf("ID = %q, want %q", c.ID, "capz")
	}
	if c.TestGrid.Dashboard != "sig-cluster-lifecycle-cluster-api-provider-azure" {
		t.Errorf("TestGrid.Dashboard = %q", c.TestGrid.Dashboard)
	}
	if c.Storage.Bucket != "kubernetes-ci-logs" {
		t.Errorf("Storage.Bucket = %q", c.Storage.Bucket)
	}
	if c.Branding.Title != "CAPZ Prow Dashboard" {
		t.Errorf("Branding.Title = %q", c.Branding.Title)
	}
	if c.Branding.SourceRepo.Name != "cluster-api-provider-azure" {
		t.Errorf("Branding.SourceRepo.Name = %q", c.Branding.SourceRepo.Name)
	}
}

func TestParseMissingRequiredFields(t *testing.T) {
	const incomplete = `
id: capz
`
	_, err := parse(strings.NewReader(incomplete))
	if err == nil {
		t.Fatalf("expected error for incomplete config, got nil")
	}
	msg := err.Error()
	wantSubstrings := []string{
		"name",
		"testgrid.dashboard",
		"storage.provider",
		"storage.bucket",
		"branding.title",
		"branding.base_path",
		"branding.site_url",
		"branding.source_repo.owner",
		"branding.source_repo.name",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(msg, w) {
			t.Errorf("error missing field %q; got: %s", w, msg)
		}
	}
}

func TestParseUnknownField(t *testing.T) {
	const withTypo = `
id: capz
name: x
unknown_field: oops
testgrid:
  dashboard: x
storage:
  provider: gcs
  bucket: x
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(withTypo))
	if err == nil {
		t.Fatalf("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unknown_field") {
		t.Errorf("error should name the unknown field; got: %v", err)
	}
}

func TestParseRejectsLegacySourcePaths(t *testing.T) {
	const legacy = `
id: x
name: x
source:
  test_infra_paths: ["config/jobs/x"]
testgrid:
  dashboard: x
storage:
  provider: gcs
  bucket: x
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(legacy))
	if err == nil {
		t.Fatal("expected error for legacy source.test_infra_paths, got nil")
	}
	if !strings.Contains(err.Error(), "test_infra_paths") {
		t.Errorf("error should mention the removed field; got: %v", err)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := parse(strings.NewReader("not: : valid"))
	if err == nil {
		t.Fatalf("expected error for invalid YAML, got nil")
	}
}

// gcswebYAML uses the gcsweb provider and bucket discovery, the Istio-style
// path: no testgrid dashboard, an explicit storage gateway.
const gcswebYAML = `
id: istio
name: "Istio"
storage:
  provider: "gcsweb"
  bucket: "istio-prow"
  base: "https://gcsweb.istio.io/s3"
  prow_base: "https://prow.istio.io/view/s3"
discovery:
  source: "bucket"
  job_filters: ["integ-"]
branding:
  title: "Istio Prow Dashboard"
  base_path: "/istio-prow-ai-dashboard"
  site_url: "https://example.github.io/istio-prow-ai-dashboard"
  source_repo:
    owner: "istio"
    name: "istio"
`

func TestParseGCSWebBucketDiscovery(t *testing.T) {
	c, err := parse(strings.NewReader(gcswebYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.EffectiveDiscoverySource() != DiscoveryBucket {
		t.Errorf("discovery source = %q, want bucket", c.EffectiveDiscoverySource())
	}
	sc := c.StorageConfig()
	if string(sc.Provider) != "gcsweb" || sc.Base != "https://gcsweb.istio.io/s3" {
		t.Errorf("storage config = %+v", sc)
	}
}

func TestValidateRequiresProvider(t *testing.T) {
	const noProvider = `
id: x
name: x
testgrid:
  dashboard: d
storage:
  bucket: "b"
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(noProvider))
	if err == nil || !strings.Contains(err.Error(), "storage.provider") {
		t.Fatalf("expected storage.provider required error, got: %v", err)
	}
}

func TestValidateGCSWebRequiresBase(t *testing.T) {
	const noBase = `
id: x
name: x
storage:
  provider: "gcsweb"
  bucket: "b"
discovery:
  source: "bucket"
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(noBase))
	if err == nil || !strings.Contains(err.Error(), "storage.base") {
		t.Fatalf("expected storage.base required error, got: %v", err)
	}
}

func TestValidateBadDiscoverySource(t *testing.T) {
	const bad = `
id: x
name: x
storage:
  provider: "gcs"
  bucket: "b"
discovery:
  source: "nonsense"
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "discovery.source") {
		t.Fatalf("expected discovery.source error, got: %v", err)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ID != "capz" {
		t.Errorf("ID = %q, want capz", c.ID)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/project.yaml")
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
}

func TestDisplayShortName(t *testing.T) {
	c := &Config{ID: "x"}
	if got := c.DisplayShortName(); got != "x" {
		t.Errorf("DisplayShortName fallback = %q, want %q", got, "x")
	}
	c.ShortName = "X-Project"
	if got := c.DisplayShortName(); got != "X-Project" {
		t.Errorf("DisplayShortName = %q, want %q", got, "X-Project")
	}
}

// validConfig returns a minimally-valid Config that Validate accepts. Tests
// below mutate it to exercise individual category-rule failure paths.
func validConfig() *Config {
	return &Config{
		ID:       "test",
		Name:     "Test",
		TestGrid: TestGrid{Dashboard: "test-dashboard"},
		Storage:  Storage{Provider: "gcs", Bucket: "test-bucket"},
		Branding: Branding{
			Title:    "Test",
			BasePath: "/test",
			SiteURL:  "https://example.com",
			SourceRepo: SourceRepo{
				Owner: "owner",
				Name:  "name",
			},
		},
	}
}

func TestValidate_BaselinePasses(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("baseline config should be valid: %v", err)
	}
}

func TestValidate_Issues(t *testing.T) {
	t.Run("partial repo rejected", func(t *testing.T) {
		c := validConfig()
		c.Issues = &Issues{Enabled: true, Repo: &SourceRepo{Owner: "only-owner"}}
		if err := c.Validate(); err == nil {
			t.Error("expected error for issues.repo missing name")
		}
	})
	t.Run("bad trigger rejected", func(t *testing.T) {
		c := validConfig()
		c.Issues = &Issues{Enabled: true, Triggers: []string{"bogus"}}
		if err := c.Validate(); err == nil {
			t.Error("expected error for invalid issues.trigger")
		}
	})
	t.Run("omitted repo defaults to source_repo", func(t *testing.T) {
		c := validConfig()
		c.Issues = &Issues{Enabled: true}
		if err := c.Validate(); err != nil {
			t.Fatalf("valid issues config rejected: %v", err)
		}
		eff := c.EffectiveIssues()
		if eff.Repo == nil || eff.Repo.Owner != "owner" || eff.Repo.Name != "name" {
			t.Errorf("repo should default to source_repo, got %+v", eff.Repo)
		}
		if !eff.HasTrigger(IssueTriggerPatterns) || !eff.HasTrigger(IssueTriggerPersistent) {
			t.Errorf("triggers should default to both, got %v", eff.Triggers)
		}
		if eff.MaxNewPerRun != 5 {
			t.Errorf("MaxNewPerRun default = %d, want 5", eff.MaxNewPerRun)
		}
	})
}

func TestValidate_CategoryRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing match", func(c *Config) {
			c.Categories = []CategoryRule{{ID: "e2e", Label: "E2E"}}
		}, "categories[0].match is required"},
		{"missing id", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", Label: "E2E"}}
		}, "categories[0].id is required"},
		{"reserved id lowercase", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "x", ID: "other", Label: "Other"}}
		}, "is reserved"},
		{"reserved id mixed case", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "x", ID: "Other", Label: "Other"}}
		}, "is reserved"},
		{"id with surrounding whitespace", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: " e2e ", Label: "E2E"}}
		}, "surrounding whitespace"},
		{"valid custom rules", func(c *Config) {
			c.Categories = []CategoryRule{
				{Match: "conformance", ID: "conformance", Label: "Conformance"},
				{Match: "e2e", ID: "custom-e2e", Label: "Custom E2E"},
			}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			assertValidate(t, c, tc.wantSub)
		})
	}
}

func TestValidate_CategoryDisplayOrder(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"unknown id rejected", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: "e2e", Label: "E2E"}}
			c.CategoryDisplayOrder = []string{"e2e", "made-up"}
		}, `"made-up" is not a declared category id`},
		{"empty entry rejected", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: "e2e", Label: "E2E"}}
			c.CategoryDisplayOrder = []string{"e2e", ""}
		}, "is empty"},
		{"other is allowed", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: "e2e", Label: "E2E"}}
			c.CategoryDisplayOrder = []string{"e2e", "other"}
		}, ""},
		{"consumer ids are honored", func(c *Config) {
			c.Categories = []CategoryRule{
				{Match: "e2e-aks", ID: "aks-e2e", Label: "AKS E2E"},
				{Match: "e2e", ID: "capz-e2e", Label: "CAPZ E2E"},
			}
			c.CategoryDisplayOrder = []string{"capz-e2e", "aks-e2e"}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			assertValidate(t, c, tc.wantSub)
		})
	}
}

func TestEffectiveCategories(t *testing.T) {
	c := validConfig()
	if got := c.EffectiveCategories(); len(got) != 0 {
		t.Errorf("expected no rules when consumer omits categories, got %d (%+v)", len(got), got)
	}
	c.Categories = []CategoryRule{{Match: "x", ID: "x", Label: "X"}}
	got := c.EffectiveCategories()
	if len(got) != 1 || got[0].ID != "x" {
		t.Errorf("expected consumer rules to be returned, got %+v", got)
	}
}

func assertValidate(t *testing.T, c *Config, wantSub string) {
	t.Helper()
	err := c.Validate()
	if wantSub == "" {
		if err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not contain %q", err.Error(), wantSub)
	}
}

func TestAgentic_Effective(t *testing.T) {
	// eff resolves agentic tuning the way the fetcher does: inline under AI.
	eff := func(a Agentic) Agentic { return (&AI{Agentic: a}).EffectiveAgentic() }

	t.Run("nil receiver returns defaults", func(t *testing.T) {
		got := (*AI)(nil).EffectiveAgentic()
		if !agenticEqual(got, DefaultAgentic) {
			t.Errorf("got %+v, want defaults %+v", got, DefaultAgentic)
		}
	})
	t.Run("zero struct returns defaults", func(t *testing.T) {
		got := eff(Agentic{})
		if !agenticEqual(got, DefaultAgentic) {
			t.Errorf("got %+v, want defaults %+v", got, DefaultAgentic)
		}
	})
	t.Run("explicit limits override defaults", func(t *testing.T) {
		got := eff(Agentic{
			MaxIters: 7,
			Timeout:  30 * time.Second,
		})
		if got.MaxIters != 7 {
			t.Errorf("MaxIters = %d, want 7", got.MaxIters)
		}
		if got.Timeout != 30*time.Second {
			t.Errorf("Timeout = %v, want 30s", got.Timeout)
		}
	})
	t.Run("SingleToolCall flips through", func(t *testing.T) {
		if eff(Agentic{}).SingleToolCall {
			t.Error("SingleToolCall should default to false")
		}
		if !eff(Agentic{SingleToolCall: true}).SingleToolCall {
			t.Error("SingleToolCall=true should pass through")
		}
	})
	t.Run("Tools list passes through", func(t *testing.T) {
		in := &AI{Agentic: Agentic{Tools: []string{"filesystem"}}}
		got := in.EffectiveAgentic()
		if !equalStrings(got.Tools, []string{"filesystem"}) {
			t.Errorf("Tools = %v, want [filesystem]", got.Tools)
		}
		// Mutate input slice; effective copy must NOT change.
		in.Agentic.Tools[0] = "mutated"
		if got.Tools[0] != "filesystem" {
			t.Errorf("EffectiveAgentic returned aliased slice; got %v after mutation", got.Tools)
		}
	})
	t.Run("empty Tools falls back to default empty", func(t *testing.T) {
		got := eff(Agentic{})
		if len(got.Tools) != 0 {
			t.Errorf("Tools = %v, want empty", got.Tools)
		}
	})
	t.Run("MinToolCalls defaults to 2", func(t *testing.T) {
		if got := eff(Agentic{}); got.MinToolCalls != 2 {
			t.Errorf("MinToolCalls = %d, want 2 (default floor on)", got.MinToolCalls)
		}
	})
	t.Run("MinToolCalls passes through when set", func(t *testing.T) {
		if got := eff(Agentic{MinToolCalls: 3}); got.MinToolCalls != 3 {
			t.Errorf("MinToolCalls = %d, want 3", got.MinToolCalls)
		}
	})
	t.Run("MinGCSBytes defaults to zero", func(t *testing.T) {
		if got := eff(Agentic{}); got.MinGCSBytes != 0 {
			t.Errorf("MinGCSBytes = %d, want 0", got.MinGCSBytes)
		}
	})
	t.Run("MinGCSBytes passes through when set", func(t *testing.T) {
		if got := eff(Agentic{MinGCSBytes: 200_000}); got.MinGCSBytes != 200_000 {
			t.Errorf("MinGCSBytes = %d, want 200000", got.MinGCSBytes)
		}
	})
	t.Run("Critique defaults to max retries 2", func(t *testing.T) {
		if got := eff(Agentic{}); got.Critique.MaxRetries == nil || *got.Critique.MaxRetries != 2 {
			t.Errorf("Critique.MaxRetries = %v, want 2 (default)", got.Critique.MaxRetries)
		}
	})
	t.Run("Critique.MaxRetries accepts explicit zero", func(t *testing.T) {
		got := eff(Agentic{Critique: AgenticCritique{MaxRetries: intPtr(0)}})
		if got.Critique.MaxRetries == nil || *got.Critique.MaxRetries != 0 {
			t.Errorf("Critique.MaxRetries = %v, want 0", got.Critique.MaxRetries)
		}
	})
	t.Run("Critique.MaxRetries passes through when set", func(t *testing.T) {
		got := eff(Agentic{Critique: AgenticCritique{MaxRetries: intPtr(5)}})
		if got.Critique.MaxRetries == nil || *got.Critique.MaxRetries != 5 {
			t.Errorf("Critique.MaxRetries = %v, want 5", got.Critique.MaxRetries)
		}
	})
}

// agenticEqual compares Agentic structs without using == because Tools is a slice.
func agenticEqual(a, b Agentic) bool {
	return a.MaxIters == b.MaxIters &&
		a.Timeout == b.Timeout &&
		a.MinToolCalls == b.MinToolCalls &&
		a.MinGCSBytes == b.MinGCSBytes &&
		a.Critique.MaxRetries != nil &&
		b.Critique.MaxRetries != nil &&
		*a.Critique.MaxRetries == *b.Critique.MaxRetries &&
		a.SingleToolCall == b.SingleToolCall &&
		equalStrings(a.Tools, b.Tools)
}

func TestAnalysisConcurrency_DefaultsToOne(t *testing.T) {
	c := validConfig()
	if got := c.AnalysisConcurrency(); got != 1 {
		t.Errorf("nil AI: AnalysisConcurrency = %d, want 1", got)
	}
	c.AI = &AI{}
	if got := c.AnalysisConcurrency(); got != 1 {
		t.Errorf("unset concurrency: AnalysisConcurrency = %d, want 1", got)
	}
	c.AI = &AI{Concurrency: 0}
	if got := c.AnalysisConcurrency(); got != 1 {
		t.Errorf("zero concurrency: AnalysisConcurrency = %d, want 1", got)
	}
	c.AI = &AI{Concurrency: -3}
	if got := c.AnalysisConcurrency(); got != 1 {
		t.Errorf("negative concurrency: AnalysisConcurrency = %d, want 1 (clamped)", got)
	}
}

func TestAnalysisConcurrency_HonorsConfiguredValue(t *testing.T) {
	c := validConfig()
	c.AI = &AI{Concurrency: 6}
	if got := c.AnalysisConcurrency(); got != 6 {
		t.Errorf("AnalysisConcurrency = %d, want 6", got)
	}
}

// TestParse_AgenticInlineFields confirms agentic tuning parses from flat keys
// directly under ai:.
func TestParse_AgenticInlineFields(t *testing.T) {
	yml := validYAML + "\nai:\n  max_iters: 20\n  tools: [filesystem]\n"
	c, err := parse(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if c.AI == nil {
		t.Fatal("AI is nil")
	}
	if c.AI.Agentic.MaxIters != 20 {
		t.Errorf("MaxIters = %d, want 20", c.AI.Agentic.MaxIters)
	}
	if !equalStrings(c.AI.Agentic.Tools, []string{"filesystem"}) {
		t.Errorf("Tools = %v, want [filesystem]", c.AI.Agentic.Tools)
	}
}

func TestParse_CritiqueMaxRetries(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		{name: "omitted", yaml: "", want: 2},
		{name: "zero", yaml: "  critique:\n    max_retries: 0\n", want: 0},
		{name: "positive", yaml: "  critique:\n    max_retries: 4\n", want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yml := validYAML
			if tc.yaml != "" {
				yml += "\nai:\n" + tc.yaml
			}
			cfg, err := parse(strings.NewReader(yml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var aiConfig *AI
			if cfg.AI != nil {
				aiConfig = cfg.AI
			}
			got := aiConfig.EffectiveAgentic()
			if got.Critique.MaxRetries == nil || *got.Critique.MaxRetries != tc.want {
				t.Errorf("MaxRetries = %v, want %d", got.Critique.MaxRetries, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParse_AgenticTimeoutField(t *testing.T) {
	yml := validYAML + "\nai:\n  timeout: 8m\n"
	c, err := parse(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.AI == nil {
		t.Fatal("AI is nil")
	}
	if c.AI.Agentic.Timeout != 8*time.Minute {
		t.Errorf("Agentic.Timeout = %v, want 8m", c.AI.Agentic.Timeout)
	}
}

func TestCategorize(t *testing.T) {
	c := &Config{Categories: []CategoryRule{
		{Match: "postsubmit", ID: "postsubmit", Label: "Postsubmit"},
		{Match: "integ", ID: "integration", Label: "Integration"},
	}}
	cases := []struct{ name, want string }{
		{"integ-ambient_istio_release-1.30", "integration"},
		{"integ-ambient_istio_release-1.30_postsubmit", "postsubmit"}, // first rule wins
		{"unit-tests", "other"},
	}
	for _, tc := range cases {
		if got := c.Categorize(tc.name); got != tc.want {
			t.Errorf("Categorize(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	// No rules means ungrouped, with an empty category instead of "other".
	if got := (&Config{}).Categorize("anything"); got != "" {
		t.Errorf("Categorize with no rules = %q, want empty", got)
	}
}

func TestEffectiveFixPRsDefaults(t *testing.T) {
	// Defaults the target repo to branding.source_repo and applies field defaults.
	c := &Config{
		Branding: Branding{SourceRepo: SourceRepo{Owner: "kubernetes-sigs", Name: "cluster-api-provider-azure"}},
		AI:       &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com"}},
	}
	got := c.EffectiveFixPRs()
	if got.Repo == nil || got.Repo.Owner != "kubernetes-sigs" || got.Repo.Name != "cluster-api-provider-azure" {
		t.Errorf("Repo = %+v, want branding.source_repo", got.Repo)
	}
	if got.MinConfidence != "high" || got.MaxFiles != 3 || got.MaxNewPerRun != 1 {
		t.Errorf("defaults wrong: %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "ai-proposed-fix" {
		t.Errorf("Labels = %v, want [ai-proposed-fix]", got.Labels)
	}
	if got.Fork == nil || *got.Fork != true {
		t.Errorf("Fork default = %v, want true", got.Fork)
	}
	if got.CritiqueRetries == nil || *got.CritiqueRetries != 1 {
		t.Errorf("CritiqueRetries default = %v, want 1", got.CritiqueRetries)
	}
	// An explicit critique_retries: 0 (disable) is preserved.
	zero := 0
	c.AI.FixPRs.CritiqueRetries = &zero
	if got2 := c.EffectiveFixPRs(); got2.CritiqueRetries == nil || *got2.CritiqueRetries != 0 {
		t.Errorf("explicit critique_retries=0 not preserved: %v", got2.CritiqueRetries)
	}
	// An explicit fork: false is preserved.
	f := false
	c.AI.FixPRs.Fork = &f
	if got2 := c.EffectiveFixPRs(); got2.Fork == nil || *got2.Fork != false {
		t.Errorf("explicit Fork=false not preserved: %v", got2.Fork)
	}
}

func TestEffectiveFixPRs_AgentRuntimeDefaults(t *testing.T) {
	c := &Config{
		Branding: Branding{SourceRepo: SourceRepo{Owner: "o", Name: "n"}},
		AI: &AI{FixPRs: &FixPRs{
			Enabled: true, AuthorName: "J", AuthorEmail: "j@e.com",
			AgentRuntime: &FixAgentRuntime{},
		}},
	}
	ar := c.EffectiveFixPRs().AgentRuntime
	if ar == nil || ar.Type != "opencode" || ar.MaxTurns != 30 {
		t.Fatalf("agent_runtime defaults wrong: %+v", ar)
	}
	if ar.AllowBash == nil || !*ar.AllowBash {
		t.Errorf("allow_bash default = %v, want true", ar.AllowBash)
	}
	// An explicit allow_bash: false is preserved.
	no := false
	c.AI.FixPRs.AgentRuntime.AllowBash = &no
	if got := c.EffectiveFixPRs().AgentRuntime; got.AllowBash == nil || *got.AllowBash {
		t.Errorf("explicit allow_bash=false not preserved: %v", got.AllowBash)
	}
}

func TestEffectiveFixPRs_NilAgentRuntimeDefaultsToOpencode(t *testing.T) {
	// A nil agent_runtime block means "opencode with defaults": the coding-agent
	// generator is the only fix path, so the effective config always resolves.
	c := &Config{
		Branding: Branding{SourceRepo: SourceRepo{Owner: "o", Name: "n"}},
		AI: &AI{FixPRs: &FixPRs{
			Enabled: true, AuthorName: "J", AuthorEmail: "j@e.com",
		}},
	}
	ar := c.EffectiveFixPRs().AgentRuntime
	if ar == nil || ar.Type != "opencode" || ar.MaxTurns != 30 || ar.AllowBash == nil || !*ar.AllowBash {
		t.Fatalf("nil agent_runtime should default to opencode: %+v", ar)
	}
}

func TestValidateFixPRsRequiresAuthor(t *testing.T) {
	base := func() *Config {
		c, err := parse(strings.NewReader(validYAML))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return c
	}
	// Enabled without author identity is rejected.
	c := base()
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "author_name and author_email") {
		t.Errorf("expected author requirement error, got %v", err)
	}
	// Enabled with author identity passes.
	c = base()
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com"}}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error with author set: %v", err)
	}
	// Partial repo is rejected.
	c = base()
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com", Repo: &SourceRepo{Owner: "x"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "fix_prs.repo requires both") {
		t.Errorf("expected partial-repo error, got %v", err)
	}
	// Negative critique_retries is rejected (0 disables, not negatives).
	c = base()
	neg := -1
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com", CritiqueRetries: &neg}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "critique_retries must be >= 0") {
		t.Errorf("expected negative-critique_retries error, got %v", err)
	}
	// An unsupported agent_runtime.type is rejected.
	c = base()
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com", AgentRuntime: &FixAgentRuntime{Type: "claude"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "agent_runtime.type") {
		t.Errorf("expected unsupported agent_runtime.type error, got %v", err)
	}
	// opencode (and empty) agent_runtime.type is accepted.
	c = base()
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com", AgentRuntime: &FixAgentRuntime{Type: "opencode", Timeout: "10m"}}}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error for opencode agent_runtime: %v", err)
	}
	// A bad agent_runtime.timeout is rejected.
	c = base()
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com", AgentRuntime: &FixAgentRuntime{Timeout: "soon"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "agent_runtime.timeout") {
		t.Errorf("expected bad-timeout error, got %v", err)
	}
}

func TestEffectiveEmailNotifications(t *testing.T) {
	tests := []struct {
		name     string
		tls      string
		port     int
		wantTLS  string
		wantPort int
	}{
		{name: "default starttls", wantTLS: EmailTLSStartTLS, wantPort: 587},
		{name: "implicit TLS", tls: EmailTLSImplicit, wantTLS: EmailTLSImplicit, wantPort: 465},
		{name: "plaintext", tls: EmailTLSNone, wantTLS: EmailTLSNone, wantPort: 25},
		{name: "explicit port", tls: EmailTLSStartTLS, port: 2525, wantTLS: EmailTLSStartTLS, wantPort: 2525},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Notifications: &Notifications{Email: &EmailNotifications{
				Enabled: true,
				To:      []string{"team@example.com"},
				SMTP:    EmailSMTP{TLS: tc.tls, Port: tc.port},
			}}}
			got, enabled := c.EffectiveEmailNotifications()
			if !enabled || got.SMTP.TLS != tc.wantTLS || got.SMTP.Port != tc.wantPort {
				t.Fatalf("enabled=%v config=%+v", enabled, got)
			}
			got.To[0] = "changed@example.com"
			if c.Notifications.Email.To[0] != "team@example.com" {
				t.Fatal("effective config mutated recipients")
			}
		})
	}

	if _, enabled := (&Config{}).EffectiveEmailNotifications(); enabled {
		t.Fatal("email should be disabled without config")
	}
}

func TestValidateEmailNotifications(t *testing.T) {
	base := func() *Config {
		c, err := parse(strings.NewReader(validYAML))
		if err != nil {
			t.Fatal(err)
		}
		c.Notifications = &Notifications{Email: &EmailNotifications{
			Enabled: true,
			From:    "Dashboard <dashboard@example.com>",
			To:      []string{"team@example.com"},
			SMTP: EmailSMTP{
				Host:     "smtp.example.com",
				Username: "dashboard@example.com",
			},
		}}
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("valid email config: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*EmailNotifications)
		want   string
	}{
		{name: "missing from", mutate: func(e *EmailNotifications) { e.From = "" }, want: "notifications.email.from"},
		{name: "invalid from", mutate: func(e *EmailNotifications) { e.From = "not-an-address" }, want: "valid email address"},
		{name: "missing recipients", mutate: func(e *EmailNotifications) { e.To = nil }, want: "at least one recipient"},
		{name: "invalid recipient", mutate: func(e *EmailNotifications) { e.To = []string{"bad"} }, want: "notifications.email.to[0]"},
		{name: "missing host", mutate: func(e *EmailNotifications) { e.SMTP.Host = "" }, want: "smtp.host"},
		{name: "invalid TLS", mutate: func(e *EmailNotifications) { e.SMTP.TLS = "sometimes" }, want: "smtp.tls"},
		{name: "invalid port", mutate: func(e *EmailNotifications) { e.SMTP.Port = 70000 }, want: "smtp.port"},
		{name: "plaintext auth", mutate: func(e *EmailNotifications) { e.SMTP.TLS = EmailTLSNone }, want: "requires encrypted SMTP"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c.Notifications.Email)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateEmailNotificationsAllowsUnauthenticatedRelay(t *testing.T) {
	c, err := parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	c.Notifications = &Notifications{Email: &EmailNotifications{
		Enabled: true,
		From:    "dashboard@example.com",
		To:      []string{"team@example.com"},
		SMTP:    EmailSMTP{Host: "relay.internal", TLS: EmailTLSNone},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("unauthenticated relay: %v", err)
	}
}

func TestParseEmailNotifications(t *testing.T) {
	yaml := validYAML + `
notifications:
  email:
    enabled: true
    action_links: true
    from: "Dashboard <dashboard@example.com>"
    to:
      - "team@example.com"
    smtp:
      host: "smtp.example.com"
      username: "dashboard@example.com"
      tls: starttls
`
	c, err := parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	email, enabled := c.EffectiveEmailNotifications()
	if !enabled || !email.ActionLinks || email.SMTP.Port != 587 || email.SMTP.Host != "smtp.example.com" || len(email.To) != 1 {
		t.Fatalf("email config = %+v enabled=%v", email, enabled)
	}
}

func TestValidateFixVerifyTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.AI = &AI{FixPRs: &FixPRs{
		Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com",
		Verify: &FixVerify{Enabled: true, Timeout: "not-a-duration"},
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "verify.timeout") {
		t.Fatalf("Validate error = %v, want verify.timeout", err)
	}
}
