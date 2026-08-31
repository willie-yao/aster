package project

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

const validYAML = `
id: capz
name: "Cluster API Provider Azure"
short_name: "CAPZ"
discovery:
  testgrid_dashboard: "sig-cluster-lifecycle-cluster-api-provider-azure"
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
	if c.Discovery.TestGridDashboard != "sig-cluster-lifecycle-cluster-api-provider-azure" {
		t.Errorf("TestGrid.Dashboard = %q", c.Discovery.TestGridDashboard)
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
		"discovery.testgrid_dashboard",
		"storage.bucket",
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
discovery:
  testgrid_dashboard: x
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
discovery:
  testgrid_dashboard: x
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
		t.Fatal("expected error for removed source block, got nil")
	}
	if !strings.Contains(err.Error(), "field source not found") {
		t.Errorf("error should mention the removed source block; got: %v", err)
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

func TestParseDefaultsGCSProviderAndBrandingTitle(t *testing.T) {
	const defaults = `
id: x
name: Example
discovery:
  testgrid_dashboard: d
storage:
  bucket: "b"
branding:
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	cfg, err := parse(strings.NewReader(defaults))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Storage.Provider != "gcs" || cfg.StorageConfig().Provider != "gcs" {
		t.Fatalf("storage provider = %q, config = %+v", cfg.Storage.Provider, cfg.StorageConfig())
	}
	if cfg.Branding.Title != "Example Prow Dashboard" {
		t.Fatalf("branding title = %q", cfg.Branding.Title)
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
		ID:        "test",
		Name:      "Test",
		Discovery: Discovery{TestGridDashboard: "test-dashboard"},
		Storage:   Storage{Provider: "gcs", Bucket: "test-bucket"},
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
	t.Run("Critique defaults to zero retries", func(t *testing.T) {
		if got := eff(Agentic{}); got.Critique.MaxRetries == nil || *got.Critique.MaxRetries != 0 {
			t.Errorf("Critique.MaxRetries = %v, want 0 (default)", got.Critique.MaxRetries)
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
		a.Critique.CachePolicy == b.Critique.CachePolicy &&
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
		{name: "omitted", yaml: "", want: 0},
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

func TestAgenticCritiqueEffectiveCachePolicy(t *testing.T) {
	zero, one := 0, 1
	for _, tc := range []struct {
		name string
		in   AgenticCritique
		want CritiqueCachePolicy
	}{
		{name: "omitted defaults to hard", in: AgenticCritique{MaxRetries: &zero}, want: CritiqueCachePolicyHard},
		{name: "retries do not change the default", in: AgenticCritique{MaxRetries: &one}, want: CritiqueCachePolicyHard},
		{name: "explicit advisory ignores retries", in: AgenticCritique{MaxRetries: &one, CachePolicy: CritiqueCachePolicyAdvisory}, want: CritiqueCachePolicyAdvisory},
		{name: "explicit hard ignores retries", in: AgenticCritique{MaxRetries: &zero, CachePolicy: CritiqueCachePolicyHard}, want: CritiqueCachePolicyHard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.EffectiveCachePolicy(); got != tc.want {
				t.Fatalf("policy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParse_CritiqueCachePolicy(t *testing.T) {
	for _, policy := range []CritiqueCachePolicy{CritiqueCachePolicyStrict, CritiqueCachePolicyHard, CritiqueCachePolicyAdvisory} {
		t.Run(string(policy), func(t *testing.T) {
			cfg, err := parse(strings.NewReader(validYAML + "\nai:\n  critique:\n    cache_policy: " + string(policy) + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.AI.EffectiveAgentic().Critique.CachePolicy; got != policy {
				t.Fatalf("policy = %q, want %q", got, policy)
			}
		})
	}

	_, err := parse(strings.NewReader(validYAML + "\nai:\n  critique:\n    cache_policy: unknown\n"))
	if err == nil || !strings.Contains(err.Error(), "ai.critique.cache_policy") {
		t.Fatalf("invalid policy error = %v", err)
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
	if got.MinConfidence != "high" || got.MaxFiles != 3 {
		t.Errorf("defaults wrong: %+v", got)
	}
	c.AI.FixPRs.MinConfidence = " Medium "
	if got2 := c.EffectiveFixPRs(); got2.MinConfidence != "medium" {
		t.Errorf("normalized min confidence = %q", got2.MinConfidence)
	}
	c.AI.FixPRs.MinConfidence = "hgh"
	if got2 := c.EffectiveFixPRs(); got2.MinConfidence != "high" {
		t.Errorf("invalid min confidence did not fail closed: %q", got2.MinConfidence)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "ai-proposed-fix" {
		t.Errorf("Labels = %v, want [ai-proposed-fix]", got.Labels)
	}
	if got.Fork == nil || *got.Fork != true {
		t.Errorf("Fork default = %v, want true", got.Fork)
	}
	// The one-shot agent-sandbox runtime pins critique retries to zero.
	if got.CritiqueRetries == nil || *got.CritiqueRetries != 0 {
		t.Errorf("CritiqueRetries default = %v, want 0", got.CritiqueRetries)
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
	if ar == nil || ar.Type != "agent-sandbox" || ar.MaxTurns != 30 {
		t.Fatalf("agent_runtime defaults wrong: %+v", ar)
	}
	if ar.AllowBash == nil || *ar.AllowBash {
		t.Errorf("allow_bash default = %v, want false", ar.AllowBash)
	}
	// An explicit allow_bash: false is preserved.
	no := false
	c.AI.FixPRs.AgentRuntime.AllowBash = &no
	if got := c.EffectiveFixPRs().AgentRuntime; got.AllowBash == nil || *got.AllowBash {
		t.Errorf("explicit allow_bash=false not preserved: %v", got.AllowBash)
	}
}

func TestEffectiveFixPRsPreservesAgentSandboxCommands(t *testing.T) {
	allowBash := false
	config := &Config{AI: &AI{FixPRs: &FixPRs{AgentRuntime: &FixAgentRuntime{
		Type: "agent-sandbox", MaxTurns: 30, AllowBash: &allowBash, Timeout: "30m",
		AllowedCommands: []FixAgentCommand{
			{Argv: []string{"go", "test", "./...", "-run", "^$"}, Timeout: "15m"},
			{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "1m"},
		},
	}}}}

	effective := config.EffectiveFixPRs().AgentRuntime
	commands, err := effective.RuntimeCommands(effective.ParsedTimeout())
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].TimeoutSeconds != 15*60 || commands[1].TimeoutSeconds != 60 {
		t.Fatalf("commands = %+v", commands)
	}
	if got := commands[0].Argv; len(got) != 5 || got[3] != "-run" || got[4] != "^$" {
		t.Fatalf("argv = %v", got)
	}
	if original := config.AI.FixPRs.AgentRuntime.AllowedCommands[0]; original.Timeout != "15m" || original.Argv[0] != "go" {
		t.Fatalf("source config mutated: %+v", original)
	}
	effective.AllowedCommands[0].Argv[0] = "changed"
	if got := config.AI.FixPRs.AgentRuntime.AllowedCommands[0].Argv[0]; got != "go" {
		t.Fatalf("effective argv aliases source config: %q", got)
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
	// Invalid confidence fails closed even when batch fix PRs are disabled.
	c = base()
	c.AI = &AI{FixPRs: &FixPRs{MinConfidence: "hgh"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "min_confidence") {
		t.Errorf("expected invalid min_confidence error, got %v", err)
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

func TestResolveAIProviderAPI(t *testing.T) {
	cfg := &Config{AI: &AI{API: AIAPIResponses, Endpoint: "https://example/v1/responses", Model: "m", ServiceTier: modelprovider.ServiceTierFlex}}
	got := cfg.ResolveAIProvider(AIAPIChatCompletions, "fallback", "fallback-model", " HIGH ")
	if got.API != AIAPIResponses || got.Endpoint != cfg.AI.Endpoint || got.Model != "m" || got.ReasoningEffort != "high" || got.ServiceTier != modelprovider.ServiceTierFlex {
		t.Fatalf("provider = %+v", got)
	}
	defaults := (&Config{}).ResolveAIProvider("", "endpoint", "model", "")
	if defaults.API != AIAPIChatCompletions {
		t.Fatalf("default API = %q", defaults.API)
	}
}

func TestValidateAIProviderRejectsUnknownReasoningEffort(t *testing.T) {
	provider := (&Config{}).ResolveAIProvider("", "endpoint", "model", "ultra")
	if err := ValidateAIProvider(provider); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("ValidateAIProvider error = %v", err)
	}
}

func TestValidateRejectsUnknownAIAPI(t *testing.T) {
	cfg := validConfig()
	cfg.AI = &AI{API: "unknown"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ai.api") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestParseValidatesInMemory(t *testing.T) {
	data := []byte(`id: test
name: Test
discovery:
  testgrid_dashboard: dashboard
storage:
  provider: gcs
  bucket: bucket
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo:
    owner: example
    name: test
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ID != "test" {
		t.Fatalf("id = %q", cfg.ID)
	}
	if _, err := Parse(append(data, []byte("unknown_field: true\n")...)); err == nil {
		t.Fatal("expected strict parsing to reject an unknown field")
	}
}

func TestEffectiveAnalysisSourceRepo(t *testing.T) {
	c := validConfig()
	got := c.EffectiveAnalysisSourceRepo()
	if got != c.Branding.SourceRepo {
		t.Fatalf("fallback source repo = %+v, want %+v", got, c.Branding.SourceRepo)
	}
	c.AI = &AI{SourceRepo: &SourceRepo{Owner: " upstream ", Name: " source "}}
	got = c.EffectiveAnalysisSourceRepo()
	if got.Owner != "upstream" || got.Name != "source" {
		t.Fatalf("explicit source repo = %+v", got)
	}
}

func TestAnalysisSourceRepoDoesNotRedirectWriteTargets(t *testing.T) {
	c := validConfig()
	c.AI = &AI{
		SourceRepo: &SourceRepo{Owner: "upstream", Name: "source"},
		FixPRs: &FixPRs{
			Enabled: true, Repo: &SourceRepo{Owner: "write", Name: "fixes"},
			AuthorName: "Jane", AuthorEmail: "jane@example.com",
		},
	}
	c.Issues = &Issues{Enabled: true, Repo: &SourceRepo{Owner: "write", Name: "issues"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.EffectiveIssues().Repo; got.Owner != "write" || got.Name != "issues" {
		t.Fatalf("issue target = %+v", got)
	}
	if got := c.EffectiveFixPRs().Repo; got.Owner != "write" || got.Name != "fixes" {
		t.Fatalf("fix target = %+v", got)
	}

	c.Issues.Repo = nil
	c.AI.FixPRs.Repo = nil
	if got := c.EffectiveIssues().Repo; *got != c.Branding.SourceRepo {
		t.Fatalf("default issue target = %+v, want branding repo", got)
	}
	if got := c.EffectiveFixPRs().Repo; *got != c.Branding.SourceRepo {
		t.Fatalf("default fix target = %+v, want branding repo", got)
	}
}

func TestValidateAnalysisSourceAndConsumerSkills(t *testing.T) {
	c := validConfig()
	c.AI = &AI{SourceRepo: &SourceRepo{Owner: "only-owner"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ai.source_repo") {
		t.Fatalf("partial source repo error = %v", err)
	}
	c.AI = &AI{ConsumerSkills: ConsumerSkills{MinimumCount: -1}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "minimum_count") {
		t.Fatalf("negative minimum error = %v", err)
	}
	c.AI = &AI{ConsumerSkills: ConsumerSkills{Required: true}}
	if got := c.EffectiveConsumerSkills(); got.MinimumCount != 1 {
		t.Fatalf("required default = %+v", got)
	}
}

func TestEffectiveAIUsage(t *testing.T) {
	if got := (*AI)(nil).EffectiveUsage(); got.Enabled || got.RetentionDays != 0 || got.RecentOperations != 0 {
		t.Fatalf("nil AI usage = %+v", got)
	}
	defaults := (&AI{}).EffectiveUsage()
	if !defaults.Enabled || defaults.RetentionDays != 90 || defaults.RecentOperations != 250 {
		t.Fatalf("defaults = %+v", defaults)
	}
	falseValue := false
	zero := 0
	configured := (&AI{Usage: &AIUsage{
		Enabled: &falseValue, RetentionDays: 30, RecentOperations: &zero,
		Pricing: &AIUsagePricing{Currency: " USD ", InputPerMillion: " 1.25 ", CacheWriteInputPerMillion: " 1.5 ", OutputPerMillion: " 10 "},
	}}).EffectiveUsage()
	if configured.Enabled || configured.RetentionDays != 30 || configured.RecentOperations != 0 {
		t.Fatalf("configured = %+v", configured)
	}
	if configured.Pricing.Currency != "USD" || configured.Pricing.CachedInputPerMillion != "1.25" || configured.Pricing.CacheWriteInputPerMillion != "1.5" {
		t.Fatalf("pricing = %+v", configured.Pricing)
	}
}

func TestValidateAIUsage(t *testing.T) {
	base := func() *Config {
		cfg, err := parse(strings.NewReader(validYAML))
		if err != nil {
			t.Fatal(err)
		}
		cfg.AI = &AI{}
		return cfg
	}
	tests := []struct {
		name    string
		usage   *AIUsage
		wantErr string
	}{
		{name: "defaults"},
		{name: "valid", usage: &AIUsage{RetentionDays: 30, RecentOperations: intPtr(0), Pricing: &AIUsagePricing{Currency: "USD", InputPerMillion: "1.25", CachedInputPerMillion: "0.125", CacheWriteInputPerMillion: "1.5", OutputPerMillion: "10"}}},
		{name: "retention", usage: &AIUsage{RetentionDays: 3651}, wantErr: "retention_days"},
		{name: "recent negative", usage: &AIUsage{RecentOperations: intPtr(-1)}, wantErr: "recent_operations"},
		{name: "recent large", usage: &AIUsage{RecentOperations: intPtr(5001)}, wantErr: "recent_operations"},
		{name: "currency", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "usd", InputPerMillion: "1", OutputPerMillion: "2"}}, wantErr: "currency"},
		{name: "numeric currency", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "123", InputPerMillion: "1", OutputPerMillion: "2"}}, wantErr: "currency"},
		{name: "symbol currency", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "$$$", InputPerMillion: "1", OutputPerMillion: "2"}}, wantErr: "currency"},
		{name: "partial", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "USD", InputPerMillion: "1"}}, wantErr: "requires input_per_million and output_per_million"},
		{name: "negative", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "USD", InputPerMillion: "-1", OutputPerMillion: "2"}}, wantErr: "non-negative decimal"},
		{name: "exponent", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "USD", InputPerMillion: "1e2", OutputPerMillion: "2"}}, wantErr: "non-negative decimal"},
		{name: "too large", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "USD", InputPerMillion: "1000000.1", OutputPerMillion: "2"}}, wantErr: "at most"},
		{name: "negative cache write", usage: &AIUsage{Pricing: &AIUsagePricing{Currency: "USD", InputPerMillion: "1", CacheWriteInputPerMillion: "-1", OutputPerMillion: "2"}}, wantErr: "cache_write_input_per_million"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := base()
			cfg.AI.Usage = testCase.usage
			err := cfg.Validate()
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, testCase.wantErr)
			}
		})
	}
}

func TestValidateExactBucketJobs(t *testing.T) {
	valid := validConfig()
	valid.Discovery = Discovery{Source: DiscoveryBucket, ExactJobs: []string{"periodic-project-e2e", "ci_project.unit"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid exact jobs rejected: %v", err)
	}

	cases := []struct {
		name      string
		discovery Discovery
		want      string
	}{
		{name: "combined filters", discovery: Discovery{Source: DiscoveryBucket, ExactJobs: []string{"job"}, JobFilters: []string{"job"}}, want: "cannot be combined"},
		{name: "unsafe", discovery: Discovery{Source: DiscoveryBucket, ExactJobs: []string{"../job"}}, want: "not a safe Prow job name"},
		{name: "whitespace", discovery: Discovery{Source: DiscoveryBucket, ExactJobs: []string{" job"}}, want: "no surrounding whitespace"},
		{name: "duplicate", discovery: Discovery{Source: DiscoveryBucket, ExactJobs: []string{"job", "job"}}, want: "duplicates"},
		{name: "testgrid", discovery: Discovery{Source: DiscoveryTestGrid, ExactJobs: []string{"job"}}, want: "requires discovery.source bucket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Discovery = tc.discovery
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateTestInfraRevision(t *testing.T) {
	valid := validConfig()
	valid.Discovery.TestInfraRevision = strings.Repeat("a", 40)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid test-infra revision rejected: %v", err)
	}

	for _, tc := range []struct {
		name      string
		revision  string
		source    string
		wantError string
	}{
		{name: "short", revision: "abcdef", wantError: "lowercase 40-character"},
		{name: "uppercase", revision: strings.Repeat("A", 40), wantError: "lowercase 40-character"},
		{name: "non hex", revision: strings.Repeat("z", 40), wantError: "lowercase 40-character"},
		{name: "whitespace", revision: " " + strings.Repeat("a", 40), wantError: "lowercase 40-character"},
		{name: "bucket", revision: strings.Repeat("a", 40), source: DiscoveryBucket, wantError: "requires discovery.source testgrid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Discovery.Source = tc.source
			cfg.Discovery.TestInfraRevision = tc.revision
			if tc.source == DiscoveryBucket {
				cfg.Discovery.TestGridDashboard = ""
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("validation error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestResolvedTestInfraRevisionCannotBeConfigured(t *testing.T) {
	_, err := parse(strings.NewReader(`
id: example
discovery:
  resolved_test_infra_revision: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`))
	if err == nil || !strings.Contains(err.Error(), "resolved_test_infra_revision") {
		t.Fatalf("parse error = %v", err)
	}
}

func TestParseTestInfraRevision(t *testing.T) {
	revision := strings.Repeat("a", 40)
	cfg, err := parse(strings.NewReader(`
id: example
name: Example
discovery:
  testgrid_dashboard: example-dashboard
  source: testgrid
  test_infra_revision: ` + revision + `
storage:
  bucket: kubernetes-ci-logs
branding:
  title: Example
  base_path: /example
  site_url: https://example.invalid
  source_repo:
    owner: example
    name: project
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discovery.TestInfraRevision != revision {
		t.Fatalf("revision = %q, want %q", cfg.Discovery.TestInfraRevision, revision)
	}
}

func TestEffectiveFixPRsAgentSandboxDefaults(t *testing.T) {
	c := &Config{AI: &AI{FixPRs: &FixPRs{AgentRuntime: &FixAgentRuntime{Type: "agent-sandbox"}}}}
	got := c.EffectiveFixPRs().AgentRuntime
	if got.Timeout != "10m" || got.MaxTurns != 30 || got.OutputLimitBytes != 512<<10 {
		t.Fatalf("agent-sandbox defaults = %+v", got)
	}
	if len(got.AllowedCommands) != 1 || !slices.Equal(got.AllowedCommands[0].Argv, []string{"git", "diff", "--cached", "--check"}) || got.AllowedCommands[0].Timeout != "1m" {
		t.Fatalf("agent-sandbox command defaults = %+v", got.AllowedCommands)
	}
	if got.AllowBash == nil || *got.AllowBash {
		t.Fatalf("agent-sandbox allow_bash default = %v, want false", got.AllowBash)
	}
	provider := got.ModelProvider.RuntimeConfig()
	if provider.CredentialMode != "direct" || provider.API != "chat_completions" || provider.Auth.Type != "none" {
		t.Fatalf("agent-sandbox provider defaults = %+v", provider)
	}
}

func validAgentSandboxModelProvider() FixModelProvider {
	return FixModelProvider{
		CredentialMode: "direct", API: "chat_completions",
		Endpoint: "https://api.githubcopilot.com/chat/completions", Model: "fixture-model", ReasoningEffort: modelprovider.ReasoningEffortHigh,
		Auth: FixModelProviderAuth{Type: "bearer"},
	}
}

func TestValidateAgentSandboxFixRuntime(t *testing.T) {
	c := validConfig()
	no := false
	c.AI = &AI{FixPRs: &FixPRs{Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com", AgentRuntime: &FixAgentRuntime{
		Type: "agent-sandbox", AllowBash: &no, MaxTurns: 30, Timeout: "90s", OutputLimitBytes: 131072,
		AllowedCommands: []FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}},
		ModelProvider:   validAgentSandboxModelProvider(),
	}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid agent-sandbox runtime rejected: %v", err)
	}
	yes := true
	c.AI.FixPRs.AgentRuntime.AllowBash = &yes
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "allow_bash") {
		t.Fatalf("agent-sandbox allow_bash error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.AllowBash = &no
	c.AI.FixPRs.AgentRuntime.AllowedCommands = nil
	c.AI.FixPRs.AgentRuntime.OutputLimitBytes = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("agent-sandbox defaults rejected: %v", err)
	}
	c.AI.FixPRs.AgentRuntime.AllowedCommands = []FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}}
	c.AI.FixPRs.AgentRuntime.OutputLimitBytes = 131072
	c.AI.FixPRs.AgentRuntime.ModelProvider.API = "responses"
	c.AI.FixPRs.AgentRuntime.ModelProvider.Endpoint = "https://api.githubcopilot.com/responses"
	if err := c.Validate(); err != nil {
		t.Fatalf("agent-sandbox Responses provider rejected: %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider.ReasoningEffort = modelprovider.ReasoningEffortMax
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "OpenCode 1.18.2") {
		t.Fatalf("agent-sandbox max reasoning effort error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	c.AI.FixPRs.AgentRuntime.ModelProvider.Endpoint = "https://api.githubcopilot.com/chat/completions"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "responses endpoint") {
		t.Fatalf("agent-sandbox API path mismatch error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider = validAgentSandboxModelProvider()
	c.AI.FixPRs.AgentRuntime.ModelProvider.Auth.Type = "none"
	if err := c.Validate(); err != nil {
		t.Fatalf("agent-sandbox unauthenticated direct provider rejected: %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider = FixModelProvider{
		CredentialMode: "gateway", API: "chat_completions",
		Endpoint: "https://gateway.fixture.svc.cluster.local/v1/chat/completions", Model: "fixture-model",
		Auth: FixModelProviderAuth{Type: "none"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("agent-sandbox gateway provider rejected: %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider.Auth.Type = "bearer"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "gateway credential mode") {
		t.Fatalf("agent-sandbox gateway bearer error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider = validAgentSandboxModelProvider()
	c.AI.FixPRs.AgentRuntime.ModelProvider.PublicCAPrivateDNS = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "gateway credential mode") {
		t.Fatalf("agent-sandbox direct trust option error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.ModelProvider = validAgentSandboxModelProvider()
	c.AI.FixPRs.AgentRuntime.AllowedCommands = []FixAgentCommand{{Argv: []string{"go", "test", "./..."}, Timeout: "30s"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must end") {
		t.Fatalf("agent-sandbox final diff command error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.AllowedCommands = []FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}}
	c.AI.FixPRs.AgentRuntime.OutputLimitBytes = 1024
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "output_limit_bytes") {
		t.Fatalf("agent-sandbox output limit error = %v", err)
	}
	c.AI.FixPRs.AgentRuntime.OutputLimitBytes = 131072
	c.AI.FixPRs.AgentRuntime.Timeout = "31m"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "at most 30m") {
		t.Fatalf("agent-sandbox timeout error = %v", err)
	}
}

func TestAgentSandboxStructuredCommandsPreserveExactArgv(t *testing.T) {
	runtime := &FixAgentRuntime{MaxTurns: 3, AllowedCommands: []FixAgentCommand{
		{Argv: []string{"validator", "argument with spaces"}, Timeout: "30s"},
		{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "1m"},
	}}
	commands, err := runtime.RuntimeCommands(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defaultTurns := &FixAgentRuntime{AllowedCommands: []FixAgentCommand{{
		Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s",
	}}}
	if _, err := defaultTurns.RuntimeCommands(2 * time.Minute); err != nil {
		t.Fatalf("default max_turns rejected: %v", err)
	}
	if got := commands[0].Argv[1]; got != "argument with spaces" {
		t.Fatalf("argv = %q", got)
	}
	for _, tc := range []struct {
		name string
		edit func(*FixAgentRuntime)
		want string
	}{
		{name: "timeout", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Timeout = "3m" }, want: "execution timeout"},
		{name: "noncanonical timeout", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Timeout = "1000ms" }, want: "whole seconds or minutes"},
		{name: "path executable", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Argv[0] = "/usr/bin/validator" }, want: "PATH-resolved"},
		{name: "dispatcher", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Argv = []string{"busybox", "sh", "-c", "true"} }, want: "command dispatcher"},
		{name: "coding agent", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Argv = []string{"opencode", "run"} }, want: "coding agent"},
		{name: "git alias shell", edit: func(r *FixAgentRuntime) {
			r.AllowedCommands[0].Argv = []string{"git", "-c", "alias.probe=!sh -c true", "probe"}
		}, want: "reserved for the exact final"},
		{name: "no generation step", edit: func(r *FixAgentRuntime) { r.MaxTurns = len(r.AllowedCommands) }, want: "reserve at least one"},
		{name: "newline", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Argv[1] = "line one\nline two" }, want: "single-line"},
		{name: "empty", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Argv[1] = "" }, want: "empty"},
		{name: "shell", edit: func(r *FixAgentRuntime) { r.AllowedCommands[0].Argv[0] = "sh" }, want: "must not invoke a shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *runtime
			copy.AllowedCommands = make([]FixAgentCommand, len(runtime.AllowedCommands))
			for i, command := range runtime.AllowedCommands {
				copy.AllowedCommands[i] = FixAgentCommand{Argv: append([]string(nil), command.Argv...), Timeout: command.Timeout}
			}
			tc.edit(&copy)
			if _, err := copy.RuntimeCommands(2 * time.Minute); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseRejectsLegacyAgentSandboxCommandStrings(t *testing.T) {
	const legacy = `
id: test
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
    name: repo
ai:
  fix_prs:
    enabled: true
    author_name: Fixture
    author_email: fixture@example.test
    critique_retries: 0
    agent_runtime:
      type: agent-sandbox
      max_turns: 2
      allow_bash: false
      timeout: 2m
      output_limit_bytes: 131072
      allowed_commands:
        - git diff --cached --check
      model_provider:
        credential_mode: gateway
        api: chat_completions
        endpoint: https://gateway.fix.svc.cluster.local/v1/chat/completions
        model: fixture
        auth:
          type: none
`
	if _, err := Parse([]byte(legacy)); err == nil || !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("legacy command error = %v", err)
	}
}

func TestAgentSandboxIsOneShotByDefault(t *testing.T) {
	config := validConfig()
	config.AI = &AI{FixPRs: &FixPRs{AgentRuntime: &FixAgentRuntime{Type: "agent-sandbox"}}}
	effective := config.EffectiveFixPRs()
	if effective.CritiqueRetries == nil || *effective.CritiqueRetries != 0 {
		t.Fatalf("critique retries = %v, want 0", effective.CritiqueRetries)
	}
	two := 2
	config.AI.FixPRs.CritiqueRetries = &two
	config.AI.FixPRs.AgentRuntime = &FixAgentRuntime{
		Type: "agent-sandbox", MaxTurns: 2, Timeout: "2m", OutputLimitBytes: 131072,
		AllowedCommands: []FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}},
		ModelProvider:   validAgentSandboxModelProvider(),
	}
	no := false
	config.AI.FixPRs.AgentRuntime.AllowBash = &no
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("critique retry validation error = %v", err)
	}
}

func TestValidateDisabledFixPRRuntimeStillFailsClosed(t *testing.T) {
	c := validConfig()
	no := false
	c.AI = &AI{FixPRs: &FixPRs{Enabled: false, AgentRuntime: &FixAgentRuntime{
		Type: "agent-sandbox", AllowBash: &no, MaxTurns: 30, Timeout: "10m", OutputLimitBytes: 131072,
		AllowedCommands: []FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}},
		ModelProvider:   FixModelProvider{CredentialMode: "gateway", API: "chat_completions", Endpoint: "https://api.openai.com/v1/chat/completions", Model: "fixture-model", Auth: FixModelProviderAuth{Type: "none"}},
	}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "public CA private DNS") {
		t.Fatalf("disabled runtime validation error = %v", err)
	}
}

func TestResolveFixDestinationAllowlist(t *testing.T) {
	fork := false
	cfg := &Config{Branding: Branding{SourceRepo: SourceRepo{Owner: "example", Name: "source"}}, AI: &AI{FixPRs: &FixPRs{
		AllowedRepositories: []FixRepository{{Owner: "kubernetes", Name: "test-infra", PathPrefixes: []string{"config/jobs/kubernetes-sigs/cluster-api-provider-azure/"}, Fork: &fork}},
	}}}
	destination, err := cfg.ResolveFixDestination("kubernetes/test-infra", "config/jobs/kubernetes-sigs/cluster-api-provider-azure/periodics.yaml")
	if err != nil || destination.Repo.Owner != "kubernetes" || destination.Repo.Name != "test-infra" || destination.Fork {
		t.Fatalf("destination=%+v err=%v", destination, err)
	}
	if _, err := cfg.ResolveFixDestination("kubernetes/test-infra", "config/jobs/other/periodics.yaml"); err == nil {
		t.Fatal("path outside allowlist was accepted")
	}
	if _, err := cfg.ResolveFixDestination("other/repo", "config/jobs/example.yaml"); err == nil {
		t.Fatal("repository outside allowlist was accepted")
	}
}

func TestValidateFixRepositoryAllowlist(t *testing.T) {
	cfg := validConfig()
	cfg.AI = &AI{FixPRs: &FixPRs{AllowedRepositories: []FixRepository{{Owner: "kubernetes", Name: "test-infra"}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "path_prefixes") {
		t.Fatalf("missing prefixes error = %v", err)
	}
	cfg.AI.FixPRs.AllowedRepositories[0].PathPrefixes = []string{"../config/"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid prefix") {
		t.Fatalf("unsafe prefix error = %v", err)
	}
}

func TestParseAgentSandboxReasoningEffort(t *testing.T) {
	cfg, err := Parse([]byte(validYAML + `
ai:
  fix_prs:
    enabled: true
    author_name: Fixture
    author_email: fixture@example.test
    critique_retries: 0
    agent_runtime:
      type: agent-sandbox
      max_turns: 3
      allow_bash: false
      timeout: 2m
      output_limit_bytes: 131072
      allowed_commands:
        - argv: [git, diff, --cached, --check]
          timeout: 30s
      model_provider:
        credential_mode: direct
        api: chat_completions
        endpoint: https://api.githubcopilot.com/chat/completions
        model: fixture
        reasoning_effort: HIGH
        auth:
          type: bearer
`))
	if err != nil {
		t.Fatal(err)
	}
	provider := cfg.EffectiveFixPRs().AgentRuntime.ModelProvider.RuntimeConfig()
	if provider.ReasoningEffort != modelprovider.ReasoningEffortHigh {
		t.Fatalf("reasoning effort = %q", provider.ReasoningEffort)
	}
}

func TestValidateRejectsLegacyLocalVerifierWithAgentSandbox(t *testing.T) {
	cfg := validConfig()
	allowBash := false
	cfg.AI = &AI{FixPRs: &FixPRs{
		Enabled: true, AuthorName: "Jane", AuthorEmail: "jane@example.com",
		Verify: &FixVerify{Enabled: true, Commands: []string{"go test ./..."}},
		AgentRuntime: &FixAgentRuntime{
			Type: "agent-sandbox", AllowBash: &allowBash, MaxTurns: 30, Timeout: "90s", OutputLimitBytes: 131072,
			AllowedCommands: []FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "30s"}},
			ModelProvider:   validAgentSandboxModelProvider(),
		},
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "use agent_runtime.allowed_commands") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestValidate_PullRequests(t *testing.T) {
	cases := []struct {
		name         string
		pullRequests *PullRequests
		wantErr      bool
	}{
		{name: "absent"},
		{name: "enabled with defaults", pullRequests: &PullRequests{Enabled: true}},
		{name: "explicit bounds", pullRequests: &PullRequests{Enabled: true, Max: 25, BuildsPerJob: 5}},
		{name: "negative max", pullRequests: &PullRequests{Enabled: true, Max: -1}, wantErr: true},
		{name: "negative builds per job", pullRequests: &PullRequests{Enabled: true, BuildsPerJob: -1}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.PullRequests = tc.pullRequests
			err := cfg.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestParsePullRequestsBlock(t *testing.T) {
	cfg, err := parse(strings.NewReader(`
id: example
name: Example
discovery:
  testgrid_dashboard: d
storage:
  provider: gcs
  bucket: b
branding:
  title: t
  base_path: /p
  site_url: https://example.test/p
  source_repo:
    owner: example
    name: project
pull_requests:
  enabled: true
  max: 25
  builds_per_job: 5
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.PullRequests == nil || !cfg.PullRequests.Enabled {
		t.Fatalf("pull requests = %+v, want enabled", cfg.PullRequests)
	}
	if cfg.PullRequests.Max != 25 || cfg.PullRequests.BuildsPerJob != 5 {
		t.Errorf("pull request bounds = %+v", cfg.PullRequests)
	}
}

func TestValidate_PullRequestComment(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			name: "negative cap rejected",
			mutate: func(c *Config) {
				c.PullRequests = &PullRequests{Enabled: true, Comment: &PullRequestComment{MaxPerPass: -1}}
			},
			wantSub: "pull_requests.comment.max_per_pass",
		},
		{
			name: "commenting without triage rejected",
			mutate: func(c *Config) {
				c.PullRequests = &PullRequests{Comment: &PullRequestComment{Enabled: true}}
			},
			wantSub: "requires pull_requests.enabled",
		},
		{
			name: "commenting with triage accepted",
			mutate: func(c *Config) {
				c.PullRequests = &PullRequests{Enabled: true, Comment: &PullRequestComment{Enabled: true}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("valid config rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// TestCommentDryRunDefaultsOn pins the safety default: the only unattended
// write that contacts a contributor's pull request must never post because a
// field was omitted.
func TestCommentDryRunDefaultsOn(t *testing.T) {
	enabled := func(dryRun *bool) *Config {
		c := validConfig()
		c.PullRequests = &PullRequests{Enabled: true, Comment: &PullRequestComment{Enabled: true, DryRun: dryRun}}
		return c
	}
	yes, no := true, false

	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "comment unconfigured", cfg: validConfig(), want: true},
		{name: "dry_run omitted", cfg: enabled(nil), want: true},
		{name: "dry_run true", cfg: enabled(&yes), want: true},
		{name: "dry_run explicitly false", cfg: enabled(&no), want: false},
		{name: "disabled with dry_run false", cfg: func() *Config {
			c := validConfig()
			c.PullRequests = &PullRequests{Enabled: true, Comment: &PullRequestComment{DryRun: &no}}
			return c
		}(), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CommentDryRun(); got != tc.want {
				t.Fatalf("CommentDryRun() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCommentEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil config", cfg: nil, want: false},
		{name: "no pull_requests block", cfg: validConfig(), want: false},
		{name: "no comment block", cfg: func() *Config {
			c := validConfig()
			c.PullRequests = &PullRequests{Enabled: true}
			return c
		}(), want: false},
		{name: "comment disabled", cfg: func() *Config {
			c := validConfig()
			c.PullRequests = &PullRequests{Enabled: true, Comment: &PullRequestComment{}}
			return c
		}(), want: false},
		{name: "comment enabled", cfg: func() *Config {
			c := validConfig()
			c.PullRequests = &PullRequests{Enabled: true, Comment: &PullRequestComment{Enabled: true}}
			return c
		}(), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CommentEnabled(); got != tc.want {
				t.Fatalf("CommentEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestValidateAIProviderServiceTier(t *testing.T) {
	valid := AIProvider{API: AIAPIResponses, Endpoint: "https://api.openai.com/v1/responses", Model: "m", ServiceTier: modelprovider.ServiceTierFlex}
	if err := ValidateAIProvider(valid); err != nil {
		t.Fatalf("valid flex provider: %v", err)
	}
	for _, provider := range []AIProvider{
		{API: AIAPIChatCompletions, Endpoint: "https://api.openai.com/v1/chat/completions", Model: "m", ServiceTier: modelprovider.ServiceTierFlex},
		{API: AIAPIResponses, Endpoint: "https://api.githubcopilot.com/responses", Model: "m", ServiceTier: modelprovider.ServiceTierFlex},
	} {
		if err := ValidateAIProvider(provider); err == nil {
			t.Fatalf("invalid flex provider passed: %+v", provider)
		}
	}
}

func TestEffectiveAgenticFlexTimeout(t *testing.T) {
	ai := &AI{ServiceTier: modelprovider.ServiceTierFlex, Agentic: Agentic{Timeout: time.Minute}}
	if got := ai.EffectiveAgentic().Timeout; got != 15*time.Minute {
		t.Fatalf("flex timeout = %v, want 15m", got)
	}
	ai.Agentic.Timeout = 20 * time.Minute
	if got := ai.EffectiveAgentic().Timeout; got != 20*time.Minute {
		t.Fatalf("explicit longer flex timeout = %v, want 20m", got)
	}
}
