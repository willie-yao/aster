package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustSkillSetJSON(t *testing.T, raw string) *Set {
	t.Helper()
	var contract struct {
		Skills []Skill `json:"skills"`
	}
	if err := json.Unmarshal([]byte(raw), &contract); err != nil {
		t.Fatal(err)
	}
	entries := make([]sourcedSkill, 0, len(contract.Skills))
	for i := range contract.Skills {
		if err := validateAndCompile(&contract.Skills[i]); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, sourcedSkill{skill: contract.Skills[i], source: "test"})
	}
	set, err := setFromSources(entries)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// writeSkill writes a single recipe file into <dir>/skills/<name>.yaml.
func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(skillsDir, name+".yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_EmptyOrMissingDirReturnsEmpty(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		got, err := Load(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || len(got.Skills()) != 0 || got.Hash() != "" {
			t.Fatalf("expected non-nil empty Set, got skills=%d hash=%q", len(got.Skills()), got.Hash())
		}
		if matches := got.Match("anything goes here"); matches != nil {
			t.Fatalf("expected no matches on empty set, got %d", len(matches))
		}
	})
	t.Run("empty dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := Load(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n := len(got.Skills()); n != 0 {
			t.Fatalf("expected 0 skills, got %d", n)
		}
	})
}

func TestLoad_ValidSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "webhook-tls", `
id: webhook-tls-failure
name: Webhook TLS failure
description: Bootstrap webhook fails with x509 errors.
triggers:
  - "(?i)x509:?\\s*certificate"
  - "(?i)webhook.*tls"
required_evidence:
  - id: cert-manager-config
    description: cert-manager Certificate or Issuer config
    any_of:
      - "config/certmanager/.*\\.ya?ml"
      - ".*certificate\\.ya?ml"
  - id: webhook-secret
    description: webhook server cert secret contents
    any_of:
      - ".*webhook.*secret.*"
procedure: |
  1. List cert-manager Certificate objects.
  2. Inspect the webhook server secret.
`)
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(set.Skills()); n != 1 {
		t.Fatalf("expected 1 skill, got %d", n)
	}
	sk := set.Skills()[0]
	if sk.ID != "webhook-tls-failure" {
		t.Errorf("ID = %q, want webhook-tls-failure", sk.ID)
	}
	if sk.Priority != defaultPriority {
		t.Errorf("Priority = %d, want default %d", sk.Priority, defaultPriority)
	}
	if len(sk.triggerREs) != 2 {
		t.Errorf("triggerREs len = %d, want 2", len(sk.triggerREs))
	}
	if len(sk.RequiredEvidence) != 2 {
		t.Fatalf("RequiredEvidence len = %d, want 2", len(sk.RequiredEvidence))
	}
	if len(sk.RequiredEvidence[0].anyOfREs) != 2 {
		t.Errorf("evidence[0].anyOfREs len = %d, want 2", len(sk.RequiredEvidence[0].anyOfREs))
	}
	if set.Hash() == "" {
		t.Error("expected non-empty hash on populated set")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantSubstr string // substring expected in error (empty = no specific check)
	}{
		{
			name: "missing id",
			body: `
triggers:
  - "foo"
`,
			wantSubstr: "missing id",
		},
		{
			name: "no triggers",
			body: `
id: no-triggers
`,
			wantSubstr: "no triggers",
		},
		{
			name: "bad trigger regex",
			body: `
id: bad-regex
triggers:
  - "[unclosed"
`,
			wantSubstr: "bad-regex",
		},
		{
			name: "bad evidence when regex",
			body: `
id: bad-when-regex
triggers: ["foo"]
required_evidence:
  - id: g1
    when: ["[unclosed"]
    any_of: ["path"]
`,
		},
		{
			name: "bad evidence regex",
			body: `
id: bad-ev-regex
triggers: ["foo"]
required_evidence:
  - id: g1
    any_of: ["[unclosed"]
			`,
		},
		{
			name: "bad evidence content any regex",
			body: `
id: bad-content-any
triggers: ["foo"]
required_evidence:
  - id: g1
    any_of: ["path"]
    content_any_of: ["[unclosed"]
`,
			wantSubstr: "content_any_of",
		},
		{
			name: "bad evidence content all regex",
			body: `
id: bad-content-all
triggers: ["foo"]
required_evidence:
  - id: g1
    any_of: ["path"]
    content_all_of: ["[unclosed"]
`,
			wantSubstr: "content_all_of",
		},
		{
			name: "empty evidence any_of",
			body: `
id: bad-ev
triggers: ["foo"]
required_evidence:
  - id: g1
`,
		},
		{
			name: "duplicate evidence id",
			body: `
id: duplicate-evidence
triggers: ["foo"]
required_evidence:
  - id: logs
    any_of: ["one"]
  - id: logs
    any_of: ["two"]
`,
			wantSubstr: "duplicate evidence id",
		},
		{
			name: "unknown field (strict yaml)",
			body: `
id: strict
triggers: ["foo"]
typo_field: oops
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSkill(t, dir, "bad", tc.body)
			_, err := Load(dir)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestLoad_RejectsDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "a", `
id: dup
triggers: ["foo"]
`)
	writeSkill(t, dir, "b", `
id: dup
triggers: ["bar"]
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error on duplicate id")
	} else if !strings.Contains(err.Error(), "duplicate skill id") {
		t.Fatalf("error %q does not mention duplicate", err)
	}
}

func TestMatch_OrdersByPriorityThenID(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "lowprio", `
id: low
priority: 50
triggers: ["foo"]
`)
	writeSkill(t, dir, "highprio", `
id: high
priority: 200
triggers: ["foo"]
`)
	writeSkill(t, dir, "default-a", `
id: aaa
triggers: ["foo"]
`)
	writeSkill(t, dir, "default-b", `
id: bbb
triggers: ["foo"]
`)
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	matches := set.Match("hello foo bar")
	if len(matches) != 4 {
		t.Fatalf("expected 4 matches, got %d", len(matches))
	}
	wantOrder := []string{"high", "aaa", "bbb", "low"}
	for i, want := range wantOrder {
		if matches[i].ID != want {
			t.Errorf("matches[%d].ID = %q, want %q", i, matches[i].ID, want)
		}
	}
}

func TestMatch_NoMatchesReturnsNil(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "only", `
id: only
triggers: ["never-appears"]
`)
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Match("text that does not match"); got != nil {
		t.Fatalf("expected nil, got %d matches", len(got))
	}
}

func TestMatch_DedupesByID_SkillNotRetriggeredOnMultipleHits(t *testing.T) {
	// A skill with two triggers that both fire on the same text
	// should still only appear once in Match output.
	dir := t.TempDir()
	writeSkill(t, dir, "dup", `
id: webhook
triggers:
  - "(?i)x509"
  - "(?i)certificate"
`)
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := set.Match("x509: bad certificate")
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
}

func TestMatch_NilSetOrEmptyText(t *testing.T) {
	var nilSet *Set
	if got := nilSet.Match("anything"); got != nil {
		t.Errorf("nil Set Match returned %d, want nil", len(got))
	}
	dir := t.TempDir()
	writeSkill(t, dir, "x", `
id: x
triggers: ["foo"]
`)
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Match(""); got != nil {
		t.Errorf("empty text Match returned %d, want nil", len(got))
	}
}

func TestEvidenceGroup_Satisfied(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "x", `
id: webhook
triggers: ["x"]
required_evidence:
  - id: certmgr
    any_of:
      - "config/certmanager/.*\\.ya?ml"
      - ".*certificate\\.ya?ml"
`)
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := set.Skills()[0].RequiredEvidence[0]

	cases := []struct {
		name  string
		reads map[string]bool
		want  bool
	}{
		{"empty reads", map[string]bool{}, false},
		{"nil reads", nil, false},
		{"unrelated reads", map[string]bool{"a/b.log": true}, false},
		{"first pattern hit", map[string]bool{"config/certmanager/issuer.yaml": true}, true},
		{"second pattern hit", map[string]bool{"foo/my-certificate.yml": true}, true},
		{"both reads, one hits", map[string]bool{
			"random.log":              true,
			"foo/my-certificate.yaml": true,
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Satisfied(tc.reads); got != tc.want {
				t.Errorf("Satisfied = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvidenceGroup_Applies(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"connectivity",
			"triggers":["connectivity"],
			"required_evidence":[
				{"id":"always","any_of":["always"]},
				{"id":"dns","when":["(?i)dns|resolver"],"any_of":["resolv\\.conf"]}
			]
		}]
	}`)
	skill := set.Skills()[0]
	if !skill.RequiredEvidence[0].Applies("service timeout") {
		t.Fatal("unconditional evidence group did not apply")
	}
	dns := skill.RequiredEvidence[1]
	if !dns.Applies("DNS resolver refused the lookup") {
		t.Fatal("conditional evidence group did not apply to DNS draft")
	}
	if dns.Applies("Service ClusterIP timed out") {
		t.Fatal("conditional evidence group applied to unrelated draft")
	}
}

func TestHash_Properties(t *testing.T) {
	// Build two recipe sets identical in content; assert hash properties.
	t.Run("deterministic across filename order", func(t *testing.T) {
		dir1 := t.TempDir()
		writeSkill(t, dir1, "a-named-first", "id: webhook\ntriggers: [\"x\"]\n")
		writeSkill(t, dir1, "b-named-second", "id: machine\ntriggers: [\"y\"]\n")
		dir2 := t.TempDir()
		writeSkill(t, dir2, "z-was-first", "id: machine\ntriggers: [\"y\"]\n")
		writeSkill(t, dir2, "y-was-second", "id: webhook\ntriggers: [\"x\"]\n")
		set1, _ := Load(dir1)
		set2, _ := Load(dir2)
		if set1.Hash() != set2.Hash() {
			t.Errorf("hash differs across filename order: %q vs %q", set1.Hash(), set2.Hash())
		}
	})
	t.Run("changes on trigger edit", func(t *testing.T) {
		dir1 := t.TempDir()
		writeSkill(t, dir1, "x", "id: webhook\ntriggers: [\"x509\"]\n")
		dir2 := t.TempDir()
		writeSkill(t, dir2, "x", "id: webhook\ntriggers: [\"x509\", \"tls\"]\n")
		set1, _ := Load(dir1)
		set2, _ := Load(dir2)
		if set1.Hash() == set2.Hash() {
			t.Error("expected hash to change after trigger edit")
		}
	})
	t.Run("changes on evidence edit", func(t *testing.T) {
		dir1 := t.TempDir()
		writeSkill(t, dir1, "x", "id: webhook\ntriggers: [\"x\"]\nrequired_evidence:\n  - id: g\n    any_of: [\"a\"]\n")
		dir2 := t.TempDir()
		writeSkill(t, dir2, "x", "id: webhook\ntriggers: [\"x\"]\nrequired_evidence:\n  - id: g\n    any_of: [\"b\"]\n")
		set1, _ := Load(dir1)
		set2, _ := Load(dir2)
		if set1.Hash() == set2.Hash() {
			t.Error("expected hash to change after evidence edit")
		}
	})
	t.Run("changes on evidence condition edit", func(t *testing.T) {
		dir1 := t.TempDir()
		writeSkill(t, dir1, "x", "id: connectivity\ntriggers: [\"x\"]\nrequired_evidence:\n  - id: g\n    when: [\"dns\"]\n    any_of: [\"a\"]\n")
		dir2 := t.TempDir()
		writeSkill(t, dir2, "x", "id: connectivity\ntriggers: [\"x\"]\nrequired_evidence:\n  - id: g\n    when: [\"resolver\"]\n    any_of: [\"a\"]\n")
		set1, _ := Load(dir1)
		set2, _ := Load(dir2)
		if set1.Hash() == set2.Hash() {
			t.Error("expected hash to change after evidence condition edit")
		}
	})
	t.Run("stable on whitespace/comment-only edits", func(t *testing.T) {
		dir1 := t.TempDir()
		writeSkill(t, dir1, "x", `
id: webhook
triggers: ["x509"]
required_evidence:
  - id: g
    any_of: ["a"]
`)
		dir2 := t.TempDir()
		writeSkill(t, dir2, "x", `
# leading comment
id: webhook
triggers:
  - "x509"
required_evidence:
  - id: g
    any_of:
      - "a"

# trailing comment
`)
		set1, _ := Load(dir1)
		set2, _ := Load(dir2)
		if set1.Hash() != set2.Hash() {
			t.Errorf("expected hash to stay equal across whitespace edits, got %q vs %q",
				set1.Hash(), set2.Hash())
		}
	})
}

func TestPlanResolvesRankedCandidatePaths(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"flatcar",
			"name":"Flatcar provider initialization",
			"priority":200,
			"triggers":["(?i)flatcar|provider.?id"],
			"required_evidence":[
				{"id":"machine-state","description":"Machine state","any_of":["(?i)^artifacts/clusters/bootstrap/resources/[^/]+/machine/.*\\.yaml$"]},
				{"id":"node-state","description":"Node state","when":["(?i)provider.?id"],"any_of":["(?i)^artifacts/clusters/[^/]+/nodes/[^/]+/node-describe\\.txt$"]},
				{"id":"dns","description":"DNS state","when":["(?i)dns"],"any_of":["(?i)resolv\\.conf$"]}
			],
			"procedure":"Compare the Machine and Node."
		}]
	}`)
	signal := "Flatcar sysext worker capz-e2e-asfxe1 has no providerID"
	paths := []string{
		"artifacts/clusters/bootstrap/resources/other/Machine/unrelated.yaml",
		"artifacts/clusters/bootstrap/resources/capz-e2e-asfxe1/Machine/capz-e2e-asfxe1-flatcar-sysext-md-0.yaml",
		"artifacts/clusters/other/nodes/node-0/node-describe.txt",
		"artifacts/clusters/capz-e2e-asfxe1-flatcar-sysext/nodes/node-1/node-describe.txt",
		"artifacts/clusters/capz-e2e-asfxe1-flatcar-sysext/nodes/node-1/resolv.conf",
	}

	plan := set.Plan(signal, paths, 1)
	if len(plan) != 1 || plan[0].ID != "flatcar" || plan[0].Procedure == "" {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan[0].RequiredEvidence) != 2 {
		t.Fatalf("groups = %+v, want machine and node only", plan[0].RequiredEvidence)
	}
	groups := map[string]PlannedEvidenceGroup{}
	for _, group := range plan[0].RequiredEvidence {
		groups[group.ID] = group
	}
	if got := groups["machine-state"].CandidatePaths; len(got) != 1 || !strings.Contains(got[0], "flatcar-sysext") {
		t.Fatalf("machine candidates = %v", got)
	}
	if got := groups["node-state"].CandidatePaths; len(got) != 1 || !strings.Contains(got[0], "flatcar-sysext") {
		t.Fatalf("node candidates = %v", got)
	}
	if _, ok := groups["dns"]; ok {
		t.Fatalf("conditional DNS group unexpectedly applied: %+v", groups["dns"])
	}
}

func TestPlanKeepsGroupsWithoutCandidatePaths(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"quota",
			"triggers":["quota"],
			"required_evidence":[{"id":"events","any_of":["events/.*quota"]}]
		}]
	}`)
	plan := set.Plan("quota exceeded", []string{"build-log.txt"}, 3)
	if len(plan) != 1 || len(plan[0].RequiredEvidence) != 1 || len(plan[0].RequiredEvidence[0].CandidatePaths) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanPreservesMatchedProcedureWithoutApplicableGroups(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"connectivity",
			"triggers":["connectivity"],
			"required_evidence":[{"id":"dns","when":["dns"],"any_of":["resolv\\.conf"]}],
			"procedure":"Inspect the relevant connectivity layer."
		}]
	}`)
	plan := set.Plan("service connectivity failed", []string{"resolv.conf"}, 3)
	if len(plan) != 1 || plan[0].ID != "connectivity" || plan[0].Procedure == "" || len(plan[0].RequiredEvidence) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanCandidateOrderIsDeterministic(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"logs",
			"triggers":["timeout"],
			"required_evidence":[{"id":"machine","any_of":["machine/.*\\.log$"]}]
		}]
	}`)
	signal := "timeout on worker-special"
	forward := []string{
		"artifacts/machine/unrelated.log",
		"artifacts/machine/worker-special.log",
		"artifacts/machine/another-worker-special.log",
	}
	reversed := []string{forward[2], forward[1], forward[0]}
	first := set.Plan(signal, forward, 3)[0].RequiredEvidence[0].CandidatePaths
	second := set.Plan(signal, reversed, 3)[0].RequiredEvidence[0].CandidatePaths
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("candidate order depends on tree order:\nforward=%v\nreversed=%v", first, second)
	}
	want := []string{
		"artifacts/machine/another-worker-special.log",
		"artifacts/machine/worker-special.log",
		"artifacts/machine/unrelated.log",
	}
	if strings.Join(first, "\n") != strings.Join(want, "\n") {
		t.Fatalf("candidate order = %v, want %v", first, want)
	}
}

func TestCoversPlanRequiresCompleteMatchingContentReads(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"profiled",
			"triggers":["profiled"],
			"required_evidence":[
				{"id":"first","any_of":["logs/(?:first|shared)\\.log$"]},
				{"id":"second","any_of":["logs/(?:second|shared)\\.log$"]}
			]
		}]
	}`)
	signal := "profiled failure"
	completePlan := set.Plan(signal, []string{"logs/first.log", "logs/second.log"}, 3)
	sharedPlan := set.Plan(signal, []string{"logs/shared.log"}, 3)
	missingCandidatePlan := set.Plan(signal, []string{"logs/first.log"}, 3)
	noCandidatePlan := set.Plan(signal, nil, 3)

	cases := []struct {
		name    string
		text    string
		plan    []PlannedSkill
		reads   map[string]bool
		covered bool
	}{
		{name: "complete", text: signal, plan: completePlan, reads: map[string]bool{"logs/first.log": true, "logs/second.log": true}, covered: true},
		{name: "shared path matches both groups", text: signal, plan: sharedPlan, reads: map[string]bool{"logs/shared.log": true}, covered: true},
		{name: "missing group", text: signal, plan: completePlan, reads: map[string]bool{"logs/first.log": true}},
		{name: "read path must match group", text: signal, plan: completePlan, reads: map[string]bool{"logs/first.log": true, "logs/unrelated.log": true}},
		{name: "unavailable group with satisfied evidence", text: signal, plan: missingCandidatePlan, reads: map[string]bool{"logs/first.log": true}, covered: true},
		{name: "all groups unavailable", text: signal, plan: noCandidatePlan, reads: map[string]bool{"logs/unrelated.log": true}},
		{name: "no matched recipe", text: "unrelated failure", plan: completePlan, reads: map[string]bool{"logs/first.log": true, "logs/second.log": true}},
		{name: "empty plan", text: signal, reads: map[string]bool{"logs/first.log": true, "logs/second.log": true}},
		{name: "empty reads", text: signal, plan: completePlan},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := set.CoversPlan(tc.text, tc.plan, tc.reads); got != tc.covered {
				t.Fatalf("CoversPlan = %t, want %t", got, tc.covered)
			}
		})
	}
}

func TestPlanCoverageClassifiesSatisfiedUnavailableAndUnmet(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"profiled",
			"triggers":["profiled"],
			"required_evidence":[
				{"id":"satisfied","any_of":["logs/satisfied\\.log$"]},
				{"id":"unavailable","any_of":["logs/unavailable\\.log$"]},
				{"id":"unmet","any_of":["logs/unmet\\.log$"]},
				{"id":"conditional","when":["other subtype"],"any_of":["logs/conditional\\.log$"]}
			]
		}]
	}`)
	signal := "profiled failure"
	plan := set.Plan(signal, []string{"logs/satisfied.log", "logs/unmet.log"}, 3)
	coverage := set.PlanCoverageWithContent(signal, plan, map[string]bool{"logs/satisfied.log": true}, nil)
	if coverage.Applicable != 3 || coverage.Satisfied != 1 || coverage.Unavailable != 1 || coverage.Unmet != 1 || coverage.Covered() {
		t.Fatalf("coverage = %+v", coverage)
	}

	coverage = set.PlanCoverageWithContent(signal, plan, map[string]bool{"logs/satisfied.log": true, "logs/unmet.log": true}, nil)
	if coverage.Applicable != 3 || coverage.Satisfied != 2 || coverage.Unavailable != 1 || coverage.Unmet != 0 || !coverage.Covered() {
		t.Fatalf("covered = %+v", coverage)
	}
}

func TestEvidenceGroupContentPredicatesRequireSameArtifact(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"aso-conversion",
			"triggers":["conversion webhook"],
			"required_evidence":[{
				"id":"failed-upgrade-log",
				"any_of":["artifacts/clusters/.*/clusterctl-upgrade\\.log"],
				"content_any_of":["(?i)ManagedClustersAgentPool","(?i)VirtualNetworks?Subnet"],
				"content_all_of":["(?i)conversion webhook","(?i)connect: connection refused"]
			}]
		}]
	}`)
	group := set.Skills()[0].RequiredEvidence[0]
	if !group.HasContentPredicates() || len(group.contentAnyREs) != 2 || len(group.contentAllREs) != 2 {
		t.Fatalf("compiled content predicates = any:%d all:%d", len(group.contentAnyREs), len(group.contentAllREs))
	}
	paths := []string{
		"artifacts/clusters/clusterctl-upgrade-management-g9706x/clusterctl-upgrade.log",
		"artifacts/clusters/clusterctl-upgrade-management-p9uqx9/clusterctl-upgrade.log",
		"artifacts/clusters/clusterctl-upgrade-management-s4c1ag/clusterctl-upgrade.log",
		"artifacts/clusters/clusterctl-upgrade-management-ttjjmj/clusterctl-upgrade.log",
	}
	failing := "conversion webhook for containerservice.azure.com/v1api20250801storage, Kind=ManagedClustersAgentPool failed: connect: connection refused"
	content := map[string][]string{
		paths[0]: {failing},
		paths[1]: {"upgrade completed successfully"},
		paths[2]: {"conversion webhook health check succeeded"},
		paths[3]: {"ManagedClustersAgentPool reconciliation completed"},
	}
	for i, artifactPath := range paths {
		got := group.SatisfiedWithContent(map[string]bool{artifactPath: true}, content)
		if got != (i == 0) {
			t.Fatalf("path %s satisfied=%t, want %t", artifactPath, got, i == 0)
		}
	}

	split := map[string][]string{
		paths[1]: {"conversion webhook ManagedClustersAgentPool"},
		paths[2]: {"connect: connection refused"},
	}
	if group.SatisfiedWithContent(map[string]bool{paths[1]: true, paths[2]: true}, split) {
		t.Fatal("content split across parallel artifacts satisfied one evidence group")
	}
}

func TestEvidenceGroupContentProofAccumulatesPositivePartialReads(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"content","triggers":["failure"],
			"required_evidence":[{
				"id":"log","any_of":["failure\\.log$"],
				"content_any_of":["ManagedClustersAgentPool"],
				"content_all_of":["conversion webhook","connection refused"]
			}]
		}]
	}`)
	group := set.Skills()[0].RequiredEvidence[0]
	artifactPath := "logs/failure.log"
	reads := map[string]bool{artifactPath: true}
	content := map[string][]string{artifactPath: {"conversion webhook"}}
	if group.SatisfiedWithContent(reads, content) {
		t.Fatal("partial content incorrectly proved absent predicates")
	}
	content[artifactPath] = append(content[artifactPath], "ManagedClustersAgentPool connection refused")
	if !group.SatisfiedWithContent(reads, content) {
		t.Fatal("positive predicates across bounded reads did not satisfy the same artifact")
	}
}

func TestPlanAndHashIncludeContentPredicates(t *testing.T) {
	base := `id: content
triggers: ["failure"]
required_evidence:
  - id: log
    any_of: ["failure\\.log$"]
    content_any_of: ["first"]
    content_all_of: ["second"]
`
	dir := t.TempDir()
	writeSkill(t, dir, "content", base)
	set, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := set.Plan("failure", []string{"logs/failure.log"}, 4)
	if len(plan) != 1 || len(plan[0].RequiredEvidence) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	group := plan[0].RequiredEvidence[0]
	if !reflect.DeepEqual(group.ContentAnyOf, []string{"first"}) || !reflect.DeepEqual(group.ContentAllOf, []string{"second"}) {
		t.Fatalf("planned content predicates = %+v", group)
	}
	firstHash := set.Hash()
	writeSkill(t, dir, "content", strings.Replace(base, "second", "changed", 1))
	changed, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Hash() == firstHash {
		t.Fatal("content predicate change did not change the skill hash")
	}
}

func TestCoversPlanRequiresContentProof(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"content","triggers":["failure"],
			"required_evidence":[{
				"id":"log","any_of":["failure\\.log$"],
				"content_all_of":["initiating error","connection refused"]
			}]
		}]
	}`)
	plan := set.Plan("failure", []string{"logs/failure.log"}, 4)
	reads := map[string]bool{"logs/failure.log": true}
	if set.CoversPlanWithContent("failure", plan, reads, map[string][]string{"logs/failure.log": {"unrelated retry"}}) {
		t.Fatal("path-only read covered a content-aware plan")
	}
	if !set.CoversPlanWithContent("failure", plan, reads, map[string][]string{"logs/failure.log": {"initiating error", "connection refused"}}) {
		t.Fatal("positive same-file content proof did not cover the plan")
	}
}

func TestEvidenceContentDoesNotFabricateAdjacencyAcrossSnippets(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"adjacency","triggers":["failure"],
			"required_evidence":[{
				"id":"log","any_of":["failure\\.log$"],
				"content_all_of":["foo\\s+bar"]
			}]
		}]
	}`)
	group := set.Skills()[0].RequiredEvidence[0]
	path := "logs/failure.log"
	reads := map[string]bool{path: true}
	if group.SatisfiedWithContent(reads, map[string][]string{path: {"foo", "bar"}}) {
		t.Fatal("separate snippets fabricated regex adjacency")
	}
	if !group.SatisfiedWithContent(reads, map[string][]string{path: {"foo bar"}}) {
		t.Fatal("one snippet with real adjacency did not satisfy the regex")
	}
}

func TestEvidenceContentKeepsCaseSensitiveArtifactIdentity(t *testing.T) {
	set := mustSkillSetJSON(t, `{
		"skills":[{
			"id":"case","triggers":["failure"],
			"required_evidence":[{
				"id":"log","any_of":["logs/foo\\.log$"],
				"content_all_of":["first signal","second signal"]
			}]
		}]
	}`)
	group := set.Skills()[0].RequiredEvidence[0]
	reads := map[string]bool{"logs/foo.log": true}
	split := map[string][]string{
		"logs/Foo.log": {"first signal"},
		"logs/foo.log": {"second signal"},
	}
	if group.SatisfiedWithContent(reads, split) {
		t.Fatal("signals from case-distinct artifacts satisfied one group")
	}
	if !group.SatisfiedWithContent(reads, map[string][]string{"logs/Foo.log": {"first signal", "second signal"}}) {
		t.Fatal("same case-preserved artifact did not accumulate all predicates")
	}
}

func TestEvidenceContentPreservesRegexWhitespaceSemantics(t *testing.T) {
	anchored := mustSkillSetJSON(t, `{
		"skills":[{"id":"anchored","triggers":["failure"],"required_evidence":[{
			"id":"log","any_of":["failure\\.log$"],"content_all_of":["^ERROR"]
		}]}]
	}`).Skills()[0].RequiredEvidence[0]
	path := "logs/failure.log"
	reads := map[string]bool{path: true}
	content := map[string][]string{path: {"  ERROR\n"}}
	if anchored.SatisfiedWithContent(reads, content) {
		t.Fatal("trimming changed ^ERROR semantics")
	}
	indented := mustSkillSetJSON(t, `{
		"skills":[{"id":"indented","triggers":["failure"],"required_evidence":[{
			"id":"log","any_of":["failure\\.log$"],"content_all_of":["^\\s+ERROR"]
		}]}]
	}`).Skills()[0].RequiredEvidence[0]
	if !indented.SatisfiedWithContent(reads, content) {
		t.Fatal("indentation-aware predicate did not match returned content")
	}
}
