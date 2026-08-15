// Package skills implements engine-owned and consumer-owned diagnostic recipes.
//
// A Skill is a YAML recipe with regex triggers, required-evidence groups, and a
// human-readable procedure quoted back to the model when evidence is missing.
// Engine profiles are embedded in this package. Consumer recipes live in
// <project_dir>/skills/*.yaml. Every selected recipe must parse and compile
// cleanly.
package skills

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// defaultPriority is assigned to any recipe that doesn't set its own
// priority. Higher priority is preferred on ties.
const defaultPriority = 100

// Profile identifies one engine-owned diagnostic recipe pack.
type Profile string

const (
	// ProfileProw contains product-level Prow artifact investigation procedures.
	ProfileProw Profile = "prow"
	// ProfileKubernetes contains provider-neutral Kubernetes investigation procedures.
	ProfileKubernetes Profile = "kubernetes"
)

// ProfileSelection controls which engine profiles join consumer recipes.
// Prow is always selected. Kubernetes follows the effective k8s tool selection.
type ProfileSelection struct {
	Kubernetes bool
}

//go:embed builtin/*/*.yaml
var builtinRecipes embed.FS

// ProfilesForTools derives engine profiles from an effective tool selection.
// An empty selection keeps the engine default of filesystem plus k8s.
func ProfilesForTools(entries []string) ProfileSelection {
	selection := ProfileSelection{Kubernetes: len(entries) == 0}
	for _, entry := range entries {
		if isKubernetesToolSelection(entry) {
			selection.Kubernetes = true
			break
		}
	}
	return selection
}

// Profiles returns selected profiles in composition order.
func (s ProfileSelection) Profiles() []Profile {
	profiles := []Profile{ProfileProw}
	if s.Kubernetes {
		profiles = append(profiles, ProfileKubernetes)
	}
	return profiles
}

// String returns selected profile names in composition order.
func (s ProfileSelection) String() string {
	profiles := s.Profiles()
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, string(profile))
	}
	return strings.Join(names, ",")
}

func isKubernetesToolSelection(entry string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if entry == "k8s" || strings.HasPrefix(entry, "k8s.") {
		return true
	}
	switch strings.ReplaceAll(entry, "_", "-") {
	case "discover-clusters", "find-my-cluster", "list-cluster-machines",
		"list-machine-logs", "discover-controllers", "resolve-controller-log":
		return true
	default:
		return false
	}
}

// Skill is one diagnostic recipe.
type Skill struct {
	// ID is the recipe identifier; must be unique within a Set. Surfaced
	// in critique feedback, so pick something human-meaningful.
	ID string `yaml:"id" json:"id"`

	// Name is an optional longer label. Defaults to ID.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Description is one-line guidance for the recipe author. Not shown
	// to the model.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// Priority orders matched recipes when more than one fires on the
	// same draft. Higher first; defaults to defaultPriority.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Triggers is the list of regex patterns ORed together to decide
	// whether the recipe matches a given draft. Compiled at Load time.
	Triggers []string `yaml:"triggers" json:"triggers"`

	// RequiredEvidence is the list of evidence groups the critique gate
	// checks once the recipe matches. Each group requires a matching read path
	// and, when configured, positive content proof from that same artifact.
	RequiredEvidence []EvidenceGroup `yaml:"required_evidence,omitempty" json:"required_evidence,omitempty"`

	// Procedure is markdown guidance quoted back to the model when the
	// recipe fires and evidence is missing. Treated as untrusted prose;
	// the engine wraps it with guidance-only framing.
	Procedure string `yaml:"procedure,omitempty" json:"procedure,omitempty"`

	// compiled triggers. Not serialized.
	triggerREs []*regexp.Regexp
}

// PlannedSkill is one matched recipe with artifact candidates resolved for its
// applicable evidence groups.
type PlannedSkill struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name,omitempty"`
	Procedure        string                 `json:"procedure,omitempty"`
	RequiredEvidence []PlannedEvidenceGroup `json:"required_evidence,omitempty"`
}

// PlannedEvidenceGroup is one evidence requirement plus matching build paths.
type PlannedEvidenceGroup struct {
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	AnyOf          []string `json:"any_of"`
	ContentAnyOf   []string `json:"content_any_of,omitempty"`
	ContentAllOf   []string `json:"content_all_of,omitempty"`
	CandidatePaths []string `json:"candidate_paths,omitempty"`
}

// EvidenceGroup is one cluster of path and optional content predicates.
type EvidenceGroup struct {
	// ID identifies the group within the recipe. Surfaced in feedback;
	// recommended kebab-case, such as cert-manager-config.
	ID string `yaml:"id" json:"id"`

	// Description is the human-readable phrase shown in feedback.
	// Defaults to ID if empty.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// When optionally limits this group to drafts that match one of these
	// patterns. An empty list makes the group apply whenever its skill matches.
	When []string `yaml:"when,omitempty" json:"when,omitempty"`

	// AnyOf is the list of regex patterns. Any single match satisfies
	// the group.
	AnyOf []string `yaml:"any_of" json:"any_of"`

	// ContentAnyOf optionally requires at least one regex to match content read
	// from the same artifact whose path satisfies AnyOf.
	ContentAnyOf []string `yaml:"content_any_of,omitempty" json:"content_any_of,omitempty"`

	// ContentAllOf optionally requires every regex to match content read from the
	// same artifact whose path satisfies AnyOf.
	ContentAllOf []string `yaml:"content_all_of,omitempty" json:"content_all_of,omitempty"`

	// compiled patterns. Not serialized.
	whenREs       []*regexp.Regexp
	anyOfREs      []*regexp.Regexp
	contentAnyREs []*regexp.Regexp
	contentAllREs []*regexp.Regexp
}

// Set is a loaded, validated, and ordered collection of recipes.
type Set struct {
	skills                []Skill
	hash                  string
	profiles              []Profile
	engineCount           int
	consumerCount         int
	consumerBundlePresent bool
}

// Skills returns recipes in load order, priority desc then ID asc. Callers may
// iterate but should not mutate.
func (s *Set) Skills() []Skill {
	if s == nil {
		return nil
	}
	return s.skills
}

// Hash returns the deterministic fingerprint of the skill set,
// suitable for cache invalidation. Returns "" for the empty set.
func (s *Set) Hash() string {
	if s == nil {
		return ""
	}
	return s.hash
}

// Profiles returns the selected engine profiles in composition order.
func (s *Set) Profiles() []Profile {
	if s == nil {
		return nil
	}
	return append([]Profile(nil), s.profiles...)
}

// EngineCount returns the number of selected engine recipes.
func (s *Set) EngineCount() int {
	if s == nil {
		return 0
	}
	return s.engineCount
}

// ConsumerCount returns the number of loaded consumer recipes.
func (s *Set) ConsumerCount() int {
	if s == nil {
		return 0
	}
	return s.consumerCount
}

// ConsumerBundlePresent reports whether the consumer skills directory exists.
func (s *Set) ConsumerBundlePresent() bool {
	return s != nil && s.consumerBundlePresent
}

// IDs returns recipe IDs in deterministic execution order.
func (s *Set) IDs() []string {
	if s == nil {
		return nil
	}
	ids := make([]string, 0, len(s.skills))
	for _, skill := range s.skills {
		ids = append(ids, skill.ID)
	}
	return ids
}

// Match returns the recipes whose triggers fire on the given text,
// ordered by priority desc then ID asc, de-duped by ID. Returns nil
// on a nil Set or an empty text.
func (s *Set) Match(text string) []Skill {
	if s == nil || text == "" || len(s.skills) == 0 {
		return nil
	}
	var out []Skill
	for _, sk := range s.skills {
		for _, re := range sk.triggerREs {
			if re.MatchString(text) {
				out = append(out, sk)
				break
			}
		}
	}
	return out
}

// Plan matches recipes against text and resolves bounded artifact candidates for
// every applicable evidence group.
func (s *Set) Plan(text string, artifactPaths []string, maxCandidates int) []PlannedSkill {
	matched := s.Match(text)
	if len(matched) == 0 {
		return nil
	}
	planned := make([]PlannedSkill, 0, len(matched))
	for _, skill := range matched {
		groups := make([]PlannedEvidenceGroup, 0, len(skill.RequiredEvidence))
		for _, group := range skill.RequiredEvidence {
			if !group.Applies(text) {
				continue
			}
			groups = append(groups, PlannedEvidenceGroup{
				ID:             group.ID,
				Description:    group.Description,
				AnyOf:          append([]string(nil), group.AnyOf...),
				ContentAnyOf:   append([]string(nil), group.ContentAnyOf...),
				ContentAllOf:   append([]string(nil), group.ContentAllOf...),
				CandidatePaths: group.CandidatePaths(text, artifactPaths, maxCandidates),
			})
		}
		planned = append(planned, PlannedSkill{
			ID: skill.ID, Name: skill.Name, Procedure: skill.Procedure, RequiredEvidence: groups,
		})
	}
	return planned
}

// PlanCoverage classifies every applicable initial evidence group. An unavailable
// group has no matching candidate in a complete artifact-tree scan. An unmet
// group has a candidate but no successful substantive matching read.
type PlanCoverage struct {
	Applicable  int
	Satisfied   int
	Unavailable int
	Unmet       int
}

// Covered reports whether all available groups were satisfied and at least one
// group contributed substantive evidence.
func (c PlanCoverage) Covered() bool {
	return c.Applicable > 0 && c.Satisfied > 0 && c.Unmet == 0
}

// CoversPlan reports whether every applicable group in the matched initial plan
// is satisfied or deterministically unavailable.
func (s *Set) CoversPlan(text string, plan []PlannedSkill, reads map[string]bool) bool {
	return s.CoversPlanWithContent(text, plan, reads, nil)
}

// CoversPlanWithContent reports initial-plan coverage using positive content
// proof captured from bounded reads. Raw content remains in-memory only. The
// caller must reject failed or truncated artifact-tree scans before using this
// result because only a complete scan can prove that a group is unavailable.
func (s *Set) CoversPlanWithContent(text string, plan []PlannedSkill, reads map[string]bool, contentByPath map[string][]string) bool {
	return s.PlanCoverageWithContent(text, plan, reads, contentByPath).Covered()
}

// PlanCoverageWithContent returns deterministic group states for a complete
// initial artifact-tree plan.
func (s *Set) PlanCoverageWithContent(text string, plan []PlannedSkill, reads map[string]bool, contentByPath map[string][]string) PlanCoverage {
	var coverage PlanCoverage
	if s == nil || strings.TrimSpace(text) == "" || len(plan) == 0 {
		return coverage
	}
	matched := s.Match(text)
	if len(matched) == 0 {
		return coverage
	}
	plannedSkills := make(map[string]PlannedSkill, len(plan))
	for _, planned := range plan {
		plannedSkills[planned.ID] = planned
	}
	for _, skill := range matched {
		planned, ok := plannedSkills[skill.ID]
		if !ok {
			coverage.Unmet++
			continue
		}
		plannedGroups := make(map[string]PlannedEvidenceGroup, len(planned.RequiredEvidence))
		for _, group := range planned.RequiredEvidence {
			plannedGroups[group.ID] = group
		}
		for _, group := range skill.RequiredEvidence {
			if !group.Applies(text) {
				continue
			}
			coverage.Applicable++
			plannedGroup, ok := plannedGroups[group.ID]
			if !ok {
				coverage.Unmet++
				continue
			}
			if len(plannedGroup.CandidatePaths) == 0 {
				coverage.Unavailable++
				continue
			}
			candidates := make(map[string]bool, len(plannedGroup.CandidatePaths))
			for _, candidate := range plannedGroup.CandidatePaths {
				if normalized := normalizeEvidencePath(candidate); normalized != "" {
					candidates[normalized] = true
				}
			}
			if group.Satisfied(candidates) && group.SatisfiedWithContent(reads, contentByPath) {
				coverage.Satisfied++
			} else {
				coverage.Unmet++
			}
		}
	}
	return coverage
}

// CandidatePaths returns the highest-signal artifact paths matching this group.
func (g EvidenceGroup) CandidatePaths(signal string, artifactPaths []string, limit int) []string {
	if len(g.anyOfREs) == 0 || len(artifactPaths) == 0 || limit <= 0 {
		return nil
	}
	tokens := evidenceSignalTokens(signal)
	type candidate struct {
		path  string
		score int
	}
	seen := map[string]bool{}
	candidates := make([]candidate, 0)
	for _, artifactPath := range artifactPaths {
		candidatePath := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(artifactPath), "./"), "/")
		normalized := normalizeEvidencePath(artifactPath)
		if normalized == "" || candidatePath == "" || seen[candidatePath] {
			continue
		}
		matched := false
		for _, re := range g.anyOfREs {
			if re.MatchString(normalized) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		seen[candidatePath] = true
		score := 0
		for _, token := range tokens {
			if strings.Contains(normalized, token) {
				score += len(token)
			}
		}
		candidates = append(candidates, candidate{path: candidatePath, score: score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].path < candidates[j].path
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out := make([]string, len(candidates))
	for i := range candidates {
		out[i] = candidates[i].path
	}
	return out
}

func evidenceSignalTokens(signal string) []string {
	seen := map[string]bool{}
	tokens := []string{}
	for _, token := range strings.FieldsFunc(strings.ToLower(signal), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(token) < 4 || seen[token] {
			continue
		}
		seen[token] = true
		tokens = append(tokens, token)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if len(tokens[i]) != len(tokens[j]) {
			return len(tokens[i]) > len(tokens[j])
		}
		return tokens[i] < tokens[j]
	})
	return tokens
}

func normalizeEvidencePath(artifactPath string) string {
	artifactPath = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(artifactPath), "\\", "/"))
	artifactPath = strings.TrimPrefix(artifactPath, "./")
	return strings.TrimPrefix(artifactPath, "/")
}

// Applies reports whether this evidence group applies to the supplied draft.
// Groups without when patterns apply whenever their parent skill matches.
func (g EvidenceGroup) Applies(text string) bool {
	if len(g.whenREs) == 0 {
		return true
	}
	for _, re := range g.whenREs {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// Satisfied reports whether the evidence group is met by the set of
// successfully-read artifact paths. reads is the same path set the
// critique gate uses for findUnreadArtifactCitations: lowercase,
// slash-normalized full paths.
func (g EvidenceGroup) Satisfied(reads map[string]bool) bool {
	if len(g.anyOfREs) == 0 || len(reads) == 0 {
		return false
	}
	for path := range reads {
		for _, re := range g.anyOfREs {
			if re.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// HasContentPredicates reports whether path evidence also needs content proof.
func (g EvidenceGroup) HasContentPredicates() bool {
	return len(g.contentAnyREs) > 0 || len(g.contentAllREs) > 0
}

// SatisfiedWithContent requires path and content predicates to match the same
// artifact. Content proof is positive-only: missing or partial content never
// proves that a pattern is absent.
func (g EvidenceGroup) SatisfiedWithContent(reads map[string]bool, contentByPath map[string][]string) bool {
	if len(g.anyOfREs) == 0 || len(reads) == 0 {
		return false
	}
	if !g.HasContentPredicates() {
		return g.Satisfied(reads)
	}
	for contentPath, snippets := range contentByPath {
		artifactPath := normalizeEvidencePath(contentPath)
		if artifactPath == "" || !reads[artifactPath] {
			continue
		}
		pathMatched := false
		for _, re := range g.anyOfREs {
			if re.MatchString(artifactPath) {
				pathMatched = true
				break
			}
		}
		if !pathMatched {
			continue
		}
		if len(snippets) == 0 {
			continue
		}
		if len(g.contentAnyREs) > 0 {
			matched := false
			for _, re := range g.contentAnyREs {
				if regexMatchesAnySnippet(re, snippets) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		allMatched := true
		for _, re := range g.contentAllREs {
			if !regexMatchesAnySnippet(re, snippets) {
				allMatched = false
				break
			}
		}
		if allMatched {
			return true
		}
	}
	return false
}

func regexMatchesAnySnippet(re *regexp.Regexp, snippets []string) bool {
	for _, snippet := range snippets {
		if re.MatchString(snippet) {
			return true
		}
	}
	return false
}

// LoadForTools selects profiles from the effective tools and loads the merged set.
func LoadForTools(dir string, toolSelection []string) (*Set, ProfileSelection, error) {
	selection := ProfilesForTools(toolSelection)
	set, err := LoadMerged(dir, selection)
	return set, selection, err
}

// LoadMerged composes selected engine profiles with consumer recipes.
// Built-ins are ordered before consumers for validation, then the complete set
// is sorted by priority descending and ID ascending.
func LoadMerged(dir string, selection ProfileSelection) (*Set, error) {
	var entries []sourcedSkill
	for _, profile := range selection.Profiles() {
		loaded, err := loadBuiltinProfile(profile)
		if err != nil {
			return nil, err
		}
		for _, skill := range loaded {
			entries = append(entries, sourcedSkill{skill: skill, source: "engine profile " + string(profile)})
		}
	}

	consumer, err := Load(dir)
	if err != nil {
		return nil, err
	}
	for _, skill := range consumer.Skills() {
		if strings.HasPrefix(skill.ID, "engine.") {
			return nil, fmt.Errorf("consumer skill id %q uses reserved engine. namespace", skill.ID)
		}
		entries = append(entries, sourcedSkill{skill: skill, source: "consumer skills"})
	}
	set, err := setFromSources(entries)
	if err != nil {
		return nil, err
	}
	set.profiles = selection.Profiles()
	set.engineCount = len(set.skills) - consumer.ConsumerCount()
	set.consumerCount = consumer.ConsumerCount()
	set.consumerBundlePresent = consumer.ConsumerBundlePresent()
	return set, nil
}

type sourcedSkill struct {
	skill  Skill
	source string
}

func loadBuiltinProfile(profile Profile) ([]Skill, error) {
	pattern := fmt.Sprintf("builtin/%s/*.yaml", profile)
	paths, err := fs.Glob(builtinRecipes, pattern)
	if err != nil {
		return nil, fmt.Errorf("load engine profile %s: %w", profile, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("engine profile %s has no recipes", profile)
	}
	sort.Strings(paths)
	loaded := make([]Skill, 0, len(paths))
	prefix := "engine." + string(profile) + "."
	for _, path := range paths {
		data, err := builtinRecipes.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("load engine recipe %s: %w", path, err)
		}
		skill, err := ParseAndValidate(data)
		if err != nil {
			return nil, fmt.Errorf("load engine recipe %s: %w", path, err)
		}
		if !strings.HasPrefix(skill.ID, prefix) {
			return nil, fmt.Errorf("engine recipe %s id %q must start with %q", path, skill.ID, prefix)
		}
		loaded = append(loaded, skill)
	}
	return loaded, nil
}

func setFromSources(entries []sourcedSkill) (*Set, error) {
	seen := map[string]string{}
	loaded := make([]Skill, 0, len(entries))
	for _, entry := range entries {
		if previous, ok := seen[entry.skill.ID]; ok {
			return nil, fmt.Errorf("duplicate skill id %q in %s and %s", entry.skill.ID, previous, entry.source)
		}
		seen[entry.skill.ID] = entry.source
		loaded = append(loaded, entry.skill)
	}
	sort.SliceStable(loaded, func(i, j int) bool {
		if loaded[i].Priority != loaded[j].Priority {
			return loaded[i].Priority > loaded[j].Priority
		}
		return loaded[i].ID < loaded[j].ID
	})
	return &Set{skills: loaded, hash: computeHash(loaded)}, nil
}

// Load reads <dir>/skills/*.{yaml,yml}, parses each as a single Skill,
// compiles every regex, and returns a Set ordered by priority desc then
// ID asc. A missing directory returns an empty Set. Read errors, YAML
// parse errors, regex compile errors, and duplicate IDs are hard errors.
func Load(dir string) (*Set, error) {
	skillsDir := filepath.Join(dir, "skills")
	info, err := os.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &Set{}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", skillsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s exists but is not a directory", skillsDir)
	}

	yamlPaths, err := filepath.Glob(filepath.Join(skillsDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s/*.yaml: %w", skillsDir, err)
	}
	ymlPaths, err := filepath.Glob(filepath.Join(skillsDir, "*.yml"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s/*.yml: %w", skillsDir, err)
	}
	paths := append(yamlPaths, ymlPaths...)
	sort.Strings(paths)
	if len(paths) == 0 {
		return &Set{consumerBundlePresent: true}, nil
	}

	seen := map[string]string{}
	var loaded []Skill
	for _, p := range paths {
		sk, err := loadOne(p)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", p, err)
		}
		if prev, ok := seen[sk.ID]; ok {
			return nil, fmt.Errorf("duplicate skill id %q in %s and %s", sk.ID, prev, p)
		}
		seen[sk.ID] = p
		loaded = append(loaded, sk)
	}

	sort.SliceStable(loaded, func(i, j int) bool {
		if loaded[i].Priority != loaded[j].Priority {
			return loaded[i].Priority > loaded[j].Priority
		}
		return loaded[i].ID < loaded[j].ID
	})

	return &Set{
		skills: loaded, hash: computeHash(loaded),
		consumerCount: len(loaded), consumerBundlePresent: true,
	}, nil
}

// loadOne reads a single recipe file, parses, validates, and compiles
// every regex.
func loadOne(path string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	sk, err := ParseAndValidate(data)
	if err != nil {
		return Skill{}, err
	}
	return sk, nil
}

// ParseAndValidate decodes one recipe from YAML, then validates and compiles
// every regex. It is the single-recipe entry point used by callers that
// generate a recipe and need to reject an invalid draft before writing it.
func ParseAndValidate(data []byte) (Skill, error) {
	var sk Skill
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&sk); err != nil {
		return Skill{}, fmt.Errorf("parsing yaml: %w", err)
	}
	if err := validateAndCompile(&sk); err != nil {
		return Skill{}, err
	}
	return sk, nil
}

// validateAndCompile checks required fields and compiles every regex
// in the recipe. Returns the recipe with triggerREs and anyOfREs
// populated.
func validateAndCompile(sk *Skill) error {
	if strings.TrimSpace(sk.ID) == "" {
		return fmt.Errorf("missing id")
	}
	if len(sk.Triggers) == 0 {
		return fmt.Errorf("skill %q has no triggers", sk.ID)
	}
	if sk.Priority == 0 {
		sk.Priority = defaultPriority
	}

	sk.triggerREs = make([]*regexp.Regexp, 0, len(sk.Triggers))
	for i, pat := range sk.Triggers {
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("skill %q trigger[%d] %q: %w", sk.ID, i, pat, err)
		}
		sk.triggerREs = append(sk.triggerREs, re)
	}

	groupIDs := map[string]bool{}
	for gi := range sk.RequiredEvidence {
		g := &sk.RequiredEvidence[gi]
		groupID := strings.TrimSpace(g.ID)
		if groupID == "" {
			return fmt.Errorf("skill %q evidence[%d] missing id", sk.ID, gi)
		}
		if groupIDs[groupID] {
			return fmt.Errorf("skill %q has duplicate evidence id %q", sk.ID, groupID)
		}
		groupIDs[groupID] = true
		if len(g.AnyOf) == 0 {
			return fmt.Errorf("skill %q evidence %q has empty any_of", sk.ID, g.ID)
		}
		g.whenREs = make([]*regexp.Regexp, 0, len(g.When))
		for i, pat := range g.When {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("skill %q evidence %q when[%d] %q: %w",
					sk.ID, g.ID, i, pat, err)
			}
			g.whenREs = append(g.whenREs, re)
		}
		g.anyOfREs = make([]*regexp.Regexp, 0, len(g.AnyOf))
		for i, pat := range g.AnyOf {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("skill %q evidence %q any_of[%d] %q: %w",
					sk.ID, g.ID, i, pat, err)
			}
			g.anyOfREs = append(g.anyOfREs, re)
		}
		g.contentAnyREs = make([]*regexp.Regexp, 0, len(g.ContentAnyOf))
		for i, pat := range g.ContentAnyOf {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("skill %q evidence %q content_any_of[%d] %q: %w",
					sk.ID, g.ID, i, pat, err)
			}
			g.contentAnyREs = append(g.contentAnyREs, re)
		}
		g.contentAllREs = make([]*regexp.Regexp, 0, len(g.ContentAllOf))
		for i, pat := range g.ContentAllOf {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("skill %q evidence %q content_all_of[%d] %q: %w",
					sk.ID, g.ID, i, pat, err)
			}
			g.contentAllREs = append(g.contentAllREs, re)
		}
	}
	return nil
}

// computeHash returns a deterministic fingerprint over the load-order-
// invariant content of the recipe set. Changes to ID, triggers,
// required-evidence patterns, or procedure flip the hash; whitespace or
// comment changes in the source YAML do not.
func computeHash(loaded []Skill) string {
	if len(loaded) == 0 {
		return ""
	}
	// Sort by ID for a load-order-invariant fingerprint.
	byID := append([]Skill(nil), loaded...)
	sort.Slice(byID, func(i, j int) bool { return byID[i].ID < byID[j].ID })

	h := sha256.New()
	for _, sk := range byID {
		fmt.Fprintf(h, "id:%s\n", sk.ID)
		fmt.Fprintf(h, "name:%s\n", sk.Name)
		fmt.Fprintf(h, "priority:%d\n", sk.Priority)
		// Trigger order matters for match semantics, so it must
		// affect the hash too.
		for _, t := range sk.Triggers {
			fmt.Fprintf(h, "trigger:%s\n", t)
		}
		for _, g := range sk.RequiredEvidence {
			fmt.Fprintf(h, "evidence-id:%s\n", g.ID)
			fmt.Fprintf(h, "evidence-desc:%s\n", g.Description)
			for _, p := range g.When {
				fmt.Fprintf(h, "evidence-when:%s\n", p)
			}
			for _, p := range g.AnyOf {
				fmt.Fprintf(h, "evidence-anyof:%s\n", p)
			}
			for _, p := range g.ContentAnyOf {
				fmt.Fprintf(h, "evidence-content-anyof:%s\n", p)
			}
			for _, p := range g.ContentAllOf {
				fmt.Fprintf(h, "evidence-content-allof:%s\n", p)
			}
		}
		fmt.Fprintf(h, "procedure:%s\n", sk.Procedure)
		h.Write([]byte("---\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
