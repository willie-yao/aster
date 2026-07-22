// Package skills implements engine-owned and consumer-owned diagnostic recipes.
//
// A Skill is a YAML recipe with regex triggers, required-evidence groups, and a
// human-readable procedure quoted back to the model when evidence is missing.
// Engine profiles are embedded in this package. Consumer recipes live in
// <project_dir>/skills/*.yaml. Every selected recipe must parse and compile
// cleanly.
package skills

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultPriority is assigned to any recipe that doesn't set its own
// priority. Higher priority is preferred on ties.
const (
	defaultPriority             = 100
	maxSkillContractBytes       = 256 << 10
	maxSkillContractHeaderBytes = 48 << 10
)

// ContractHeader carries a serialized merged skill set to external tools.
const ContractHeader = "X-Prow-AI-Skills"

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
	// checks once the recipe matches. Each group is satisfied if any of
	// its any_of regexes matches any path the agent successfully read.
	RequiredEvidence []EvidenceGroup `yaml:"required_evidence,omitempty" json:"required_evidence,omitempty"`

	// Procedure is markdown guidance quoted back to the model when the
	// recipe fires and evidence is missing. Treated as untrusted prose;
	// the engine wraps it with guidance-only framing.
	Procedure string `yaml:"procedure,omitempty" json:"procedure,omitempty"`

	// compiled triggers. Not serialized.
	triggerREs []*regexp.Regexp
}

// EvidenceGroup is one OR'd cluster of artifact-path regex patterns. A
// draft satisfies the group iff at least one regex matches at least one
// artifact path the agent successfully read.
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

	// compiled patterns. Not serialized.
	whenREs  []*regexp.Regexp
	anyOfREs []*regexp.Regexp
}

// Set is a loaded, validated, and ordered collection of recipes.
type Set struct {
	skills []Skill
	hash   string
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

// Contract is the transport-safe representation used by external analysis backends.
type Contract struct {
	Hash   string  `json:"hash,omitempty"`
	Skills []Skill `json:"skills,omitempty"`
}

// MarshalContract serializes the validated skill set without compiled regexes.
func (s *Set) MarshalContract() ([]byte, error) {
	contract := Contract{}
	if s != nil {
		contract.Hash = s.hash
		contract.Skills = append([]Skill(nil), s.skills...)
	}
	return json.Marshal(contract)
}

// HeaderValue encodes the skill contract for an HTTP Tool header.
func (s *Set) HeaderValue() (string, error) {
	if s == nil || len(s.skills) == 0 {
		return "", nil
	}
	data, err := s.MarshalContract()
	if err != nil {
		return "", err
	}
	if len(data) > maxSkillContractBytes {
		return "", fmt.Errorf("skill contract is %d bytes, exceeds %d", len(data), maxSkillContractBytes)
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(data); err != nil {
		return "", fmt.Errorf("compress skill contract: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("compress skill contract: %w", err)
	}
	header := base64.RawURLEncoding.EncodeToString(compressed.Bytes())
	if len(header) > maxSkillContractHeaderBytes {
		return "", fmt.Errorf("compressed skill contract header is %d bytes, exceeds %d", len(header), maxSkillContractHeaderBytes)
	}
	return header, nil
}

// ParseHeader decodes a skill contract from an HTTP Tool header.
func ParseHeader(value string) (*Set, error) {
	if strings.TrimSpace(value) == "" {
		return &Set{}, nil
	}
	if len(value) > maxSkillContractHeaderBytes {
		return nil, fmt.Errorf("skill contract header exceeds %d bytes", maxSkillContractHeaderBytes)
	}
	compressed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode skill contract header: %w", err)
	}
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("decompress skill contract header: %w", err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxSkillContractBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress skill contract header: %w", err)
	}
	if len(data) > maxSkillContractBytes {
		return nil, fmt.Errorf("skill contract exceeds %d bytes", maxSkillContractBytes)
	}
	return ParseContract(data)
}

// ParseContract validates and compiles a serialized skill set.
func ParseContract(data []byte) (*Set, error) {
	var contract Contract
	if err := json.Unmarshal(data, &contract); err != nil {
		return nil, fmt.Errorf("parse skill contract: %w", err)
	}
	seen := map[string]bool{}
	loaded := append([]Skill(nil), contract.Skills...)
	for i := range loaded {
		if err := validateAndCompile(&loaded[i]); err != nil {
			return nil, fmt.Errorf("skill contract entry %d: %w", i, err)
		}
		if seen[loaded[i].ID] {
			return nil, fmt.Errorf("duplicate skill id %q", loaded[i].ID)
		}
		seen[loaded[i].ID] = true
	}
	sort.SliceStable(loaded, func(i, j int) bool {
		if loaded[i].Priority != loaded[j].Priority {
			return loaded[i].Priority > loaded[j].Priority
		}
		return loaded[i].ID < loaded[j].ID
	})
	hash := computeHash(loaded)
	if contract.Hash != "" && contract.Hash != hash {
		return nil, fmt.Errorf("skill contract hash mismatch")
	}
	return &Set{skills: loaded, hash: hash}, nil
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

// LoadForTools selects profiles from the effective tools and loads the merged set.
// Both analysis backends use this entry point to keep their contracts identical.
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
	return setFromSources(entries)
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
		return &Set{}, nil
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

	return &Set{skills: loaded, hash: computeHash(loaded)}, nil
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

	for gi := range sk.RequiredEvidence {
		g := &sk.RequiredEvidence[gi]
		if strings.TrimSpace(g.ID) == "" {
			return fmt.Errorf("skill %q evidence[%d] missing id", sk.ID, gi)
		}
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
		}
		fmt.Fprintf(h, "procedure:%s\n", sk.Procedure)
		h.Write([]byte("---\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
