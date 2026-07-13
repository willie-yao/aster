// Package project loads and validates the per-project YAML config that
// describes which Prow/TestGrid data the dashboard should aggregate and
// how to brand the resulting site.
//
// The fetcher loads one Config at startup and serializes the same struct to
// data/manifest.json for the frontend.
package project

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Config is the in-memory representation of a project.yaml file.
type Config struct {
	ID                   string         `yaml:"id"         json:"id"`
	Name                 string         `yaml:"name"       json:"name"`
	ShortName            string         `yaml:"short_name" json:"short_name,omitempty"`
	Source               Source         `yaml:"source"     json:"source"`
	TestGrid             TestGrid       `yaml:"testgrid,omitempty"   json:"testgrid,omitempty"`
	Storage              Storage        `yaml:"storage"    json:"storage"`
	Discovery            Discovery      `yaml:"discovery,omitempty"  json:"discovery,omitempty"`
	Branding             Branding       `yaml:"branding"   json:"branding"`
	Categories           []CategoryRule `yaml:"categories,omitempty"            json:"categories,omitempty"`
	CategoryDisplayOrder []string       `yaml:"category_display_order,omitempty" json:"category_display_order,omitempty"`
	AI                   *AI            `yaml:"ai,omitempty"         json:"ai,omitempty"`
	Issues               *Issues        `yaml:"issues,omitempty"     json:"issues,omitempty"`

	// ShortNamePrefix is a display-only hint derived at fetch time.
	// It is the longest "periodic-<x>-" prefix shared by most periodic jobs.
	// The frontend strips it from job names for compact rendering.
	ShortNamePrefix string `yaml:"-" json:"short_name_prefix,omitempty"`
}

// CategoryRule maps a substring in a job name to a category id and display
// label. Rules are evaluated in order; first match wins. When no rule
// matches, the job is categorized as "other".
//
// Rule order controls categorization, not display order. Use
// `category_display_order` when the two need to diverge.
type CategoryRule struct {
	// Match is the substring to look for in the job name. Comparison is
	// case-insensitive on both sides.
	Match string `yaml:"match" json:"match"`
	// ID is the category identifier used in JobSummary.Category and as the
	// key in dashboard grouping.
	ID string `yaml:"id" json:"id"`
	// Label is the human-readable section header rendered by the frontend.
	Label string `yaml:"label" json:"label"`
}

// EffectiveCategories returns the consumer's category rules. Categories are
// opt-in. When c.Categories is empty, the dashboard renders a flat grid and
// Categorize leaves every job's Category empty.
// Consumers who want a per-section layout declare rules in project.yaml.
func (c *Config) EffectiveCategories() []CategoryRule {
	return c.Categories
}

// Categorize returns the category id for a job name using the config's rules.
// See CategorizeJob for the matching semantics.
func (c *Config) Categorize(name string) string {
	return CategorizeJob(name, c.Categories)
}

// CategorizeJob returns the category id for a job name by evaluating rules in
// order. The first case-insensitive substring match wins. It returns "" when no
// rules are configured and "other" when rules exist but none match.
func CategorizeJob(name string, rules []CategoryRule) string {
	if len(rules) == 0 {
		return ""
	}
	lower := strings.ToLower(name)
	for _, r := range rules {
		if r.Match == "" || r.ID == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(r.Match)) {
			return r.ID
		}
	}
	return "other"
}

// Source controls fetcher behavior for kubernetes/test-infra job discovery.
// Discovery lists YAML under config/jobs/ at one test-infra commit, then keeps
// jobs whose testgrid-dashboards annotation contains cfg.TestGrid.Dashboard.
type Source struct {
	IncludePresubmits bool `yaml:"include_presubmits" json:"include_presubmits,omitempty"`
}

// TestGrid identifies the testgrid dashboard that owns the project's jobs.
// Only used when discovery.source is "testgrid".
type TestGrid struct {
	Dashboard string `yaml:"dashboard" json:"dashboard"`
}

// Storage configures the artifact store that holds the project's Prow builds.
// The engine does not assume Google Cloud Storage. Provider is required and
// selects the backend, and the optional *Base fields point the engine at a
// project's own endpoints.
//
//	provider: gcs    -> native Google Cloud Storage.
//	provider: gcsweb -> a gcsweb HTTP gateway fronting a bucket.
type Storage struct {
	Provider string `yaml:"provider" json:"provider"`
	Bucket   string `yaml:"bucket"   json:"bucket"`
	// Base is the gcsweb gateway root serving raw objects and HTML listings.
	// Required for the gcsweb provider.
	Base string `yaml:"base,omitempty" json:"base,omitempty"`
	// WebBase overrides the human-browsable link root.
	WebBase string `yaml:"web_base,omitempty" json:"web_base,omitempty"`
	// ProwBase overrides the Prow deck deep-link root.
	ProwBase string `yaml:"prow_base,omitempty" json:"prow_base,omitempty"`
}

// Discovery selects how the fetcher finds the project's jobs.
//
//	source: testgrid -> kubernetes/test-infra job YAMLs filtered by dashboard.
//	source: bucket             -> list the storage bucket's own job indexes
//	                              under logs/ and pr-logs/directory/.
type Discovery struct {
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	// JobFilters, when set, keeps only discovered job names that contain one
	// of these substrings. Only used by the bucket source; omit to take every
	// job in the bucket.
	JobFilters []string `yaml:"job_filters,omitempty" json:"job_filters,omitempty"`
}

// Discovery source names.
const (
	DiscoveryTestGrid = "testgrid"
	DiscoveryBucket   = "bucket"
)

// EffectiveDiscoverySource returns the configured discovery source, defaulting
// to "testgrid" when unset.
func (c *Config) EffectiveDiscoverySource() string {
	if c.Discovery.Source == "" {
		return DiscoveryTestGrid
	}
	return c.Discovery.Source
}

// StorageConfig maps the project's storage block onto a storage.Config.
// Validate guarantees Provider is set, so no defaulting happens here.
func (c *Config) StorageConfig() storage.Config {
	return storage.Config{
		Provider: storage.Provider(c.Storage.Provider),
		Bucket:   c.Storage.Bucket,
		Base:     c.Storage.Base,
		WebBase:  c.Storage.WebBase,
		ProwBase: c.Storage.ProwBase,
	}
}

// Branding controls UI-facing strings and URLs.
type Branding struct {
	Title      string     `yaml:"title"       json:"title"`
	BasePath   string     `yaml:"base_path"   json:"base_path"`
	SiteURL    string     `yaml:"site_url"    json:"site_url"`
	SourceRepo SourceRepo `yaml:"source_repo" json:"source_repo"`
}

// SourceRepo points at the GitHub repo whose code these tests exercise.
// It builds "view in source" deep links from failure stack traces.
type SourceRepo struct {
	Owner string `yaml:"owner" json:"owner"`
	Name  string `yaml:"name"  json:"name"`
}

// Issue trigger names.
const (
	IssueTriggerPatterns   = "patterns"   // systemic recurring patterns
	IssueTriggerPersistent = "persistent" // failures with >=3 consecutive runs
)

// Issues configures optional auto-filing of GitHub issues for recurring
// patterns and persistent failures. Off by default; the fetcher only acts when
// `enabled: true` and an ISSUE_TOKEN secret is present, so a misconfigured repo
// or a missing token is a no-op rather than a deploy failure.
type Issues struct {
	// Enabled turns the feature on for this consumer. Defaults to false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repo is the target repo for filed issues. Defaults to branding.source_repo.
	// Point it at a repo you control with an issues:write token.
	Repo *SourceRepo `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Triggers selects which signals open an issue. Defaults to both when empty.
	Triggers []string `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// Labels are applied to every filed issue. Defaults to ["prow-dashboard"].
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// CommentOnRecovery posts a "recovered" comment when a tracked failure
	// clears. Defaults to true.
	CommentOnRecovery *bool `yaml:"comment_on_recovery,omitempty" json:"comment_on_recovery,omitempty"`
	// CloseOnRecovery also closes the issue on recovery. Defaults to false.
	CloseOnRecovery bool `yaml:"close_on_recovery,omitempty" json:"close_on_recovery,omitempty"`
	// MaxNewPerRun caps how many issues are created in a single fetch, a flood
	// guard for when many patterns appear at once or local state is lost.
	// Defaults to 5.
	MaxNewPerRun int `yaml:"max_new_per_run,omitempty" json:"max_new_per_run,omitempty"`
}

// EffectiveIssues resolves the issues config with defaults applied. Safe on a
// nil receiver.
func (c *Config) EffectiveIssues() Issues {
	out := Issues{}
	if c != nil && c.Issues != nil {
		out = *c.Issues
	}
	// Default the target repo to branding.source_repo only when `repo` is
	// omitted entirely. A partial repo is rejected by
	// Validate rather than silently completed from source_repo, which could
	// file issues on the wrong repo.
	if out.Repo == nil {
		if c != nil {
			out.Repo = &SourceRepo{Owner: c.Branding.SourceRepo.Owner, Name: c.Branding.SourceRepo.Name}
		}
	}
	if len(out.Triggers) == 0 {
		out.Triggers = []string{IssueTriggerPatterns, IssueTriggerPersistent}
	}
	if len(out.Labels) == 0 {
		out.Labels = []string{"prow-dashboard"}
	}
	if out.CommentOnRecovery == nil {
		t := true
		out.CommentOnRecovery = &t
	}
	if out.MaxNewPerRun <= 0 {
		out.MaxNewPerRun = 5
	}
	return out
}

// HasTrigger reports whether the given trigger is enabled.
func (i Issues) HasTrigger(name string) bool {
	for _, t := range i.Triggers {
		if t == name {
			return true
		}
	}
	return false
}

// AI configures the agentic failure-analysis pipeline: the endpoint and model
// to call, optional request headers, analysis concurrency, and the inlined
// agentic loop tuning.
type AI struct {
	// Endpoint is the OpenAI-compatible chat-completions URL. Required when AI is
	// enabled because the engine has no default provider. Falls back to
	// AI_ENDPOINT when unset here. Excluded from manifest.json.
	Endpoint string `yaml:"endpoint,omitempty" json:"-"`

	// Model is the model identifier the provider expects. Required when AI is
	// enabled; falls back to the AI_MODEL env var when unset here. Excluded
	// from manifest.json.
	Model string `yaml:"model,omitempty" json:"-"`

	// Headers are extra HTTP headers merged into every AI request after
	// the defaults. Use for provider-specific routing headers or to
	// override the default Authorization scheme. Do not put secrets here;
	// AI_TOKEN is the supported channel for the bearer token. Never
	// serialized: the config is published as manifest.json, and a header
	// could carry a provider credential (e.g. an api-key header).
	Headers map[string]string `yaml:"headers,omitempty" json:"-"`

	// Concurrency caps how many failures are analyzed in parallel. Each analysis
	// is independent, so batching endpoints can process several investigations at
	// once. Defaults to 1 because the engine has no request-level backoff and
	// shared providers can 429 under parallelism. Excluded from manifest.json.
	Concurrency int `yaml:"concurrency,omitempty" json:"-"`

	// Agentic holds tool-calling loop tuning inlined under `ai:` in YAML.
	// Unset fields fall back to DefaultAgentic.
	// The agentic loop is the only analysis path, and a function-calling
	// endpoint is required.
	Agentic Agentic `yaml:",inline" json:"agentic,omitempty"`

	// SuggestSkills configures optional auto-drafting of skill recipes for
	// systemic recurring patterns. Off by default. Excluded from manifest.json.
	SuggestSkills *SuggestSkills `yaml:"suggest_skills,omitempty" json:"-"`

	// FixPRs configures optional auto-drafting of fix PRs against the source
	// repo for systemic recurring patterns. Off by default. Excluded from
	// manifest.json.
	FixPRs *FixPRs `yaml:"fix_prs,omitempty" json:"-"`
}

// AIProvider is the resolved provider configuration used to construct clients.
type AIProvider struct {
	Endpoint string
	Model    string
	Headers  map[string]string
}

// ResolveAIProvider applies environment fallbacks to the project configuration.
func (c *Config) ResolveAIProvider(endpointFallback, modelFallback string) AIProvider {
	out := AIProvider{Endpoint: endpointFallback, Model: modelFallback}
	if c == nil || c.AI == nil {
		return out
	}
	if c.AI.Endpoint != "" {
		out.Endpoint = c.AI.Endpoint
	}
	if c.AI.Model != "" {
		out.Model = c.AI.Model
	}
	out.Headers = c.AI.Headers
	return out
}

// SuggestSkills configures the self-improving skills feature. When a systemic
// recurring pattern is found that no existing skill covers, the engine drafts a
// skill recipe and opens a draft PR adding it to the dashboard repo's skills/
// directory. Off by default; the fetcher only acts when `enabled: true` and a
// write-capable GitHub token is present, so a missing token is a no-op rather
// than a deploy failure.
type SuggestSkills struct {
	// Enabled turns the feature on for this consumer. Defaults to false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MinConfidence is the lowest pattern confidence that qualifies.
	// Only systemic patterns are ever considered.
	MinConfidence string `yaml:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	// MaxNewPerRun caps how many suggestion PRs are opened in a single fetch.
	// Defaults to 1 because skill review is human-paced.
	MaxNewPerRun int `yaml:"max_new_per_run,omitempty" json:"max_new_per_run,omitempty"`
	// Labels are applied to every suggestion PR. Defaults to ["skill-suggestion"].
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

// EffectiveSuggestSkills resolves the skill-suggestion config with defaults
// applied. Safe on a nil receiver.
func (c *Config) EffectiveSuggestSkills() SuggestSkills {
	out := SuggestSkills{}
	if c != nil && c.AI != nil && c.AI.SuggestSkills != nil {
		out = *c.AI.SuggestSkills
	}
	if strings.TrimSpace(out.MinConfidence) == "" {
		out.MinConfidence = "high"
	}
	if out.MaxNewPerRun <= 0 {
		out.MaxNewPerRun = 1
	}
	if len(out.Labels) == 0 {
		out.Labels = []string{"skill-suggestion"}
	}
	return out
}

// FixPRs configures the agent-proposed fix-PR feature: when a systemic recurring
// pattern carries a concrete remediation, the engine drafts a minimal code edit
// and opens a draft PR against the source repo via fork-and-PR. Off by default;
// the fetcher only acts when `enabled: true`, a FIX_TOKEN secret is present, and
// the author identity is set, so a missing piece is a no-op rather than a deploy
// failure. Targets a community repo, so the commit author must be a CLA-signed
// identity (see docs/fix-prs.md).
type FixPRs struct {
	// Enabled turns the feature on for this consumer. Defaults to false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repo is the source repo to open fix PRs against. Defaults to
	// branding.source_repo.
	Repo *SourceRepo `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Fork controls how the fix branch reaches the source repo. true (default)
	// uses fork-and-PR (for a repo you don't own); false pushes the branch
	// directly and opens a same-repo PR (for a repo you own). Excluded from
	// manifest.json.
	Fork *bool `yaml:"fork,omitempty" json:"-"`
	// AuthorName / AuthorEmail are the commit author identity. Required when
	// enabled: for a community repo the commits must be authored by a CLA-signed
	// identity whose email matches that GitHub account (EasyCLA/DCO).
	AuthorName  string `yaml:"author_name,omitempty" json:"author_name,omitempty"`
	AuthorEmail string `yaml:"author_email,omitempty" json:"author_email,omitempty"`
	// MinConfidence is the lowest pattern confidence that qualifies. Only
	// systemic patterns are ever considered. Defaults to "high".
	MinConfidence string `yaml:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	// MaxFiles caps how many files a single proposed fix may touch. Defaults
	// to 3 to keep changes minimal and reviewable.
	MaxFiles int `yaml:"max_files,omitempty" json:"max_files,omitempty"`
	// MaxNewPerRun caps how many fix PRs are opened in a single fetch.
	// Defaults to 1.
	MaxNewPerRun int `yaml:"max_new_per_run,omitempty" json:"max_new_per_run,omitempty"`
	// Labels are applied to every fix PR. Defaults to ["ai-proposed-fix"].
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// DryRun runs the full generation pipeline (locate, fetch, edit, validate)
	// and records the proposed change without opening any PR. Defaults to false.
	DryRun bool `yaml:"dry_run,omitempty" json:"dry_run,omitempty"`
	// CritiqueRetries bounds how many times the edit step is re-prompted to
	// resolve an LLM reviewer's objections (or a validation error) before the
	// fix is dropped. Defaults to 1; 0 disables the review. Excluded from
	// manifest.json.
	CritiqueRetries *int `yaml:"critique_retries,omitempty" json:"-"`
	// Verify builds and vets a proposed fix in a local Runtime before the PR is
	// opened, recording the verdict on the PR and preview. Off by default; it
	// needs a git and language toolchain on the runner, which the distroless
	// image lacks (verification then reports "skipped"). Excluded from
	// manifest.json.
	Verify *FixVerify `yaml:"verify,omitempty" json:"-"`
}

// FixVerify configures pre-PR verification of a proposed fix.
type FixVerify struct {
	// Enabled turns verification on. Defaults to false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Commands override the default verification. Each entry is one command line
	// split on spaces (no shell). Defaults to `go build ./...` then
	// `go vet ./...`.
	Commands []string `yaml:"commands,omitempty" json:"-"`
	// Timeout bounds each command, e.g. "10m". Empty uses the Runtime default.
	Timeout string `yaml:"timeout,omitempty" json:"-"`
}

// ParsedCommands splits each configured command line into argv. An empty result
// tells the caller to use its built-in default.
func (v *FixVerify) ParsedCommands() [][]string {
	if v == nil {
		return nil
	}
	out := make([][]string, 0, len(v.Commands))
	for _, c := range v.Commands {
		if f := strings.Fields(c); len(f) > 0 {
			out = append(out, f)
		}
	}
	return out
}

// ParsedTimeout returns the per-command timeout, or 0 when unset or invalid.
func (v *FixVerify) ParsedTimeout() time.Duration {
	if v == nil {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(v.Timeout))
	if err != nil {
		return 0
	}
	return d
}

// EffectiveFixPRs resolves the fix-PR config with defaults applied. The target
// repo defaults to branding.source_repo when `repo` is omitted entirely. Safe on
// a nil receiver.
func (c *Config) EffectiveFixPRs() FixPRs {
	out := FixPRs{}
	if c != nil && c.AI != nil && c.AI.FixPRs != nil {
		out = *c.AI.FixPRs
	}
	if out.Repo == nil && c != nil {
		out.Repo = &SourceRepo{Owner: c.Branding.SourceRepo.Owner, Name: c.Branding.SourceRepo.Name}
	}
	if out.Fork == nil {
		t := true
		out.Fork = &t
	}
	if strings.TrimSpace(out.MinConfidence) == "" {
		out.MinConfidence = "high"
	}
	if out.MaxFiles <= 0 {
		out.MaxFiles = 3
	}
	if out.MaxNewPerRun <= 0 {
		out.MaxNewPerRun = 1
	}
	if len(out.Labels) == 0 {
		out.Labels = []string{"ai-proposed-fix"}
	}
	if out.CritiqueRetries == nil {
		n := 1
		out.CritiqueRetries = &n
	}
	return out
}

// AnalysisConcurrency returns the number of failures to analyze in parallel,
// clamped to a minimum of 1 so unset or invalid values stay sequential.
func (c *Config) AnalysisConcurrency() int {
	if c.AI == nil || c.AI.Concurrency < 1 {
		return 1
	}
	return c.AI.Concurrency
}

// Agentic configures the tool-calling AI loop. All fields are optional; zero
// values fall back to DefaultAgentic. It is inlined under `ai:` in project.yaml.
type Agentic struct {
	// MaxIters caps the number of tool-call rounds per failure. Defaults
	// to DefaultAgentic.MaxIters.
	MaxIters int `yaml:"max_iters,omitempty" json:"max_iters,omitempty"`

	// The model-output, context compaction, and GCS byte budgets are not
	// configurable. Model and context budgets derive from the endpoint's
	// reported context window; the GCS ceiling is a fixed safety cap.

	// Timeout caps the total wall-clock time spent in the agentic loop
	// per failure. When hit, the in-flight request is cancelled and the
	// analysis errors out. Defaults to DefaultAgentic.Timeout.
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// MinToolCalls is the minimum number of tool calls the model must
	// make before its final JSON answer is accepted. When the model
	// returns a tools-free response below this floor, the loop nudges it
	// to investigate further. Below-floor finals are published but not cached.
	// Defaults to 2.
	MinToolCalls int `yaml:"min_tool_calls,omitempty" json:"min_tool_calls,omitempty"`

	// MinGCSBytes is the minimum cumulative bytes the model must fetch
	// via tool calls before its final answer is accepted. Complements
	// MinToolCalls because cheap list calls can satisfy a calls floor.
	// Below-floor finals are published but not cached, like MinToolCalls.
	// Defaults to 0, which disables the floor.
	MinGCSBytes int `yaml:"min_gcs_bytes,omitempty" json:"min_gcs_bytes,omitempty"`

	// Critique tunes the always-on critique gate. After the agentic loop
	// produces a parseable tools-free final, a deterministic regex checks
	// suggested_fix; punting drafts get targeted feedback and retry up to
	// MaxRetries times, and a critique retry fetches cited-but-unread artifacts
	// and injects capped content into the feedback. Drafts that still punt are
	// published but not cached. Recipes under <project_dir>/skills/*.yaml feed
	// the gate whenever present.
	Critique AgenticCritique `yaml:"critique,omitempty" json:"critique,omitempty"`

	// SingleToolCall makes the loop execute at most one tool call per assistant
	// turn. Extra tool calls are dropped, and the model can request them later.
	// Use it for endpoints whose chat template rejects multiple tool calls in one
	// assistant message. Leave it off for providers that support parallel tool
	// calls so they keep their round-trip efficiency.
	SingleToolCall bool `yaml:"single_tool_call,omitempty" json:"single_tool_call,omitempty"`

	// Artifact-tree seeding is always on and not configurable. It is
	// deterministic, capped, and a no-op when the listing is empty.

	// Tools selects registered tool groups or individual tool names exposed to
	// the model. When empty, the fetcher enables filesystem and k8s. Non-K8s
	// projects should set ["filesystem"] to avoid empty k8s probes.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// AgenticCritique tunes the always-on critique gate. See Agentic.Critique for
// the operational semantics.
type AgenticCritique struct {
	// MaxRetries caps the number of extra re-prompt rounds the loop
	// spends per analysis when critique fails. Each retry consumes one
	// extra agentic iteration on top of MaxIters. Defaults to 2.
	MaxRetries *int `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
}

// MarshalJSON omits zero retry values from the published manifest.
func (c AgenticCritique) MarshalJSON() ([]byte, error) {
	value := 0
	if c.MaxRetries != nil {
		value = *c.MaxRetries
	}
	return json.Marshal(struct {
		MaxRetries int `json:"max_retries,omitempty"`
	}{MaxRetries: value})
}

// DefaultAgentic is the zero-config fallback for agentic loop tuning.
// The iteration and timeout defaults allow deep exploration while bounding
// runaway loops. Byte budgets are derived or fixed in fetcher wiring.
var DefaultAgentic = Agentic{
	MaxIters:     15,
	Timeout:      5 * time.Minute,
	MinToolCalls: 2,
	MinGCSBytes:  0,
	Critique: AgenticCritique{
		MaxRetries: intPtr(2),
	},
}

// EffectiveAgentic returns agentic tuning with defaults applied to unset fields.
// Safe to call on a nil receiver.
func (a *AI) EffectiveAgentic() Agentic {
	out := DefaultAgentic
	out.Critique.MaxRetries = intPtr(*DefaultAgentic.Critique.MaxRetries)
	if a == nil {
		return out
	}
	if a.Agentic.MaxIters > 0 {
		out.MaxIters = a.Agentic.MaxIters
	}
	if a.Agentic.Timeout > 0 {
		out.Timeout = a.Agentic.Timeout
	}
	if a.Agentic.MinToolCalls > 0 {
		out.MinToolCalls = a.Agentic.MinToolCalls
	}
	if a.Agentic.MinGCSBytes > 0 {
		out.MinGCSBytes = a.Agentic.MinGCSBytes
	}
	if a.Agentic.Critique.MaxRetries != nil && *a.Agentic.Critique.MaxRetries >= 0 {
		out.Critique.MaxRetries = a.Agentic.Critique.MaxRetries
	}
	out.SingleToolCall = a.Agentic.SingleToolCall
	if len(a.Agentic.Tools) > 0 {
		out.Tools = append([]string(nil), a.Agentic.Tools...)
	}
	return out
}

// Load reads and validates a project.yaml file from disk.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// LoadPrompt reads the required consumer AI prompt.
func LoadPrompt(dir string) (string, error) {
	promptPath := filepath.Join(dir, "prompts", "system.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		return "", fmt.Errorf("AI analysis requires %s; see https://github.com/willie-yao/prow-ai-dashboard/blob/main/docs/writing-prompts.md (%w)", promptPath, err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("AI analysis requires non-empty %s; see https://github.com/willie-yao/prow-ai-dashboard/blob/main/docs/writing-prompts.md", promptPath)
	}
	return prompt, nil
}

// LoadDir reads project.yaml and the required consumer AI prompt.
func LoadDir(dir string) (*Config, string, error) {
	cfg, err := Load(filepath.Join(dir, "project.yaml"))
	if err != nil {
		return nil, "", err
	}
	prompt, err := LoadPrompt(dir)
	if err != nil {
		return nil, "", err
	}
	return cfg, prompt, nil
}

func intPtr(v int) *int { return &v }

// parse decodes YAML in strict mode and runs validation.
func parse(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate reports every missing required field in one error message so
// users can fix the YAML in a single pass instead of iterating.
func (c *Config) Validate() error {
	var missing []string
	require := func(name, val string) {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	require("id", c.ID)
	require("name", c.Name)
	require("storage.provider", c.Storage.Provider)
	if c.Storage.Provider != string(storage.ProviderLocal) {
		require("storage.bucket", c.Storage.Bucket)
	}
	require("branding.title", c.Branding.Title)
	require("branding.base_path", c.Branding.BasePath)
	require("branding.site_url", c.Branding.SiteURL)
	require("branding.source_repo.owner", c.Branding.SourceRepo.Owner)
	require("branding.source_repo.name", c.Branding.SourceRepo.Name)

	switch c.EffectiveDiscoverySource() {
	case DiscoveryTestGrid:
		require("testgrid.dashboard", c.TestGrid.Dashboard)
	case DiscoveryBucket:
		// No testgrid dashboard needed; jobs come from the bucket itself.
	default:
		missing = append(missing, fmt.Sprintf("discovery.source %q (want %q or %q)",
			c.Discovery.Source, DiscoveryTestGrid, DiscoveryBucket))
	}

	switch c.Storage.Provider {
	case "", string(storage.ProviderGCS):
		// Empty is already reported above; gcs needs no extra fields.
	case string(storage.ProviderGCSWeb):
		require("storage.base (required for the gcsweb provider)", c.Storage.Base)
	case string(storage.ProviderLocal):
		require("storage.base (the root directory, required for the local provider)", c.Storage.Base)
	default:
		missing = append(missing, fmt.Sprintf("storage.provider %q (want %q, %q, or %q)",
			c.Storage.Provider, storage.ProviderGCS, storage.ProviderGCSWeb, storage.ProviderLocal))
	}

	if len(missing) > 0 {
		return fmt.Errorf("project config missing required field(s): %s", strings.Join(missing, ", "))
	}

	for i, r := range c.Categories {
		match := strings.TrimSpace(r.Match)
		id := strings.TrimSpace(r.ID)
		if match == "" {
			return fmt.Errorf("categories[%d].match is required", i)
		}
		if id == "" {
			return fmt.Errorf("categories[%d].id is required", i)
		}
		if id != r.ID {
			return fmt.Errorf("categories[%d].id %q must not have surrounding whitespace", i, r.ID)
		}
		if strings.EqualFold(id, "other") {
			return fmt.Errorf("categories[%d].id %q is reserved for the implicit fallback bucket", i, r.ID)
		}
	}

	if len(c.CategoryDisplayOrder) > 0 {
		known := map[string]struct{}{"other": {}}
		for _, r := range c.Categories {
			known[r.ID] = struct{}{}
		}
		for i, id := range c.CategoryDisplayOrder {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("category_display_order[%d] is empty", i)
			}
			if _, ok := known[id]; !ok {
				return fmt.Errorf("category_display_order[%d] %q is not a declared category id", i, id)
			}
		}
	}

	// Validate issue triggers when the feature is configured, so a typo fails
	// at load rather than silently never firing.
	if c.Issues != nil {
		for i, t := range c.Issues.Triggers {
			switch t {
			case IssueTriggerPatterns, IssueTriggerPersistent:
			default:
				return fmt.Errorf("issues.triggers[%d] %q is not valid (want %q or %q)",
					i, t, IssueTriggerPatterns, IssueTriggerPersistent)
			}
		}
		// A partial repo would otherwise be silently completed from
		// branding.source_repo, risking issues on the wrong repo.
		if r := c.Issues.Repo; r != nil && (r.Owner == "" || r.Name == "") {
			return fmt.Errorf("issues.repo requires both owner and name (omit issues.repo entirely to default to branding.source_repo)")
		}
	}

	// fix_prs targets a (usually community) source repo, so an enabled config
	// must name the CLA-signed commit author and may not carry a partial repo.
	if c.AI != nil && c.AI.FixPRs != nil && c.AI.FixPRs.Enabled {
		f := c.AI.FixPRs
		if strings.TrimSpace(f.AuthorName) == "" || strings.TrimSpace(f.AuthorEmail) == "" {
			return fmt.Errorf("ai.fix_prs requires author_name and author_email (the CLA-signed identity that authors the commits)")
		}
		if r := f.Repo; r != nil && (r.Owner == "" || r.Name == "") {
			return fmt.Errorf("ai.fix_prs.repo requires both owner and name (omit it to default to branding.source_repo)")
		}
		if f.CritiqueRetries != nil && *f.CritiqueRetries < 0 {
			return fmt.Errorf("ai.fix_prs.critique_retries must be >= 0 (0 disables the review)")
		}
	}

	return nil
}

// DisplayShortName returns ShortName, falling back to ID when unset.
func (c *Config) DisplayShortName() string {
	if c.ShortName != "" {
		return c.ShortName
	}
	return c.ID
}
