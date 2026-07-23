package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestProfilesForTools(t *testing.T) {
	tests := []struct {
		name       string
		tools      []string
		kubernetes bool
	}{
		{name: "default", kubernetes: true},
		{name: "filesystem only", tools: []string{"filesystem"}},
		{name: "k8s group", tools: []string{"filesystem", "k8s"}, kubernetes: true},
		{name: "in-process k8s tool", tools: []string{"filesystem", "k8s.discover_clusters"}, kubernetes: true},
		{name: "orka k8s tool", tools: []string{"read-artifact", "discover-clusters"}, kubernetes: true},
		{name: "orka underscored alias", tools: []string{"resolve_controller_log"}, kubernetes: true},
		{name: "explicit filesystem tool", tools: []string{"read-artifact"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProfilesForTools(tt.tools)
			if got.Kubernetes != tt.kubernetes {
				t.Fatalf("Kubernetes = %v, want %v", got.Kubernetes, tt.kubernetes)
			}
			wantProfiles := []Profile{ProfileProw}
			if tt.kubernetes {
				wantProfiles = append(wantProfiles, ProfileKubernetes)
			}
			if !reflect.DeepEqual(got.Profiles(), wantProfiles) {
				t.Fatalf("Profiles = %v, want %v", got.Profiles(), wantProfiles)
			}
		})
	}
}

func TestLoadMergedDeterministicAcrossConsumerFileOrder(t *testing.T) {
	writeConsumer := func(t *testing.T, names []string) string {
		t.Helper()
		dir := t.TempDir()
		skillsDir := filepath.Join(dir, "skills")
		if err := os.MkdirAll(skillsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		bodies := map[string]string{
			"alpha": "id: consumer-alpha\npriority: 205\ntriggers: ['alpha']\n",
			"beta":  "id: consumer-beta\npriority: 115\ntriggers: ['beta']\n",
		}
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(skillsDir, name+".yaml"), []byte(bodies[name]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	first, err := LoadMerged(writeConsumer(t, []string{"alpha", "beta"}), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadMerged(writeConsumer(t, []string{"beta", "alpha"}), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() != second.Hash() {
		t.Fatalf("hash differs across consumer file order: %q vs %q", first.Hash(), second.Hash())
	}
	if !reflect.DeepEqual(skillIDs(first.Skills()), skillIDs(second.Skills())) {
		t.Fatalf("skill order differs: %v vs %v", skillIDs(first.Skills()), skillIDs(second.Skills()))
	}
	if !containsSkill(first, "engine.prow.failure-evidence") ||
		!containsSkill(first, "engine.kubernetes.machine-node-providerid") ||
		!containsSkill(first, "consumer-alpha") {
		t.Fatalf("merged IDs = %v", skillIDs(first.Skills()))
	}
	for i := 1; i < len(first.Skills()); i++ {
		previous, current := first.Skills()[i-1], first.Skills()[i]
		if previous.Priority < current.Priority ||
			(previous.Priority == current.Priority && previous.ID > current.ID) {
			t.Fatalf("skills are not deterministically ordered at %q then %q", previous.ID, current.ID)
		}
	}
}

func TestBuiltinRecipeEditChangesMergedHash(t *testing.T) {
	prow, err := loadBuiltinProfile(ProfileProw)
	if err != nil {
		t.Fatal(err)
	}
	baseEntries := make([]sourcedSkill, 0, len(prow))
	for _, skill := range prow {
		baseEntries = append(baseEntries, sourcedSkill{skill: skill, source: "engine profile prow"})
	}
	base, err := setFromSources(baseEntries)
	if err != nil {
		t.Fatal(err)
	}

	editedEntries := append([]sourcedSkill(nil), baseEntries...)
	editedEntries[0].skill.Procedure += "\nRead one more canonical artifact."
	edited, err := setFromSources(editedEntries)
	if err != nil {
		t.Fatal(err)
	}
	if base.Hash() == edited.Hash() {
		t.Fatal("built-in procedure edit did not change the merged hash")
	}
}

func TestLoadMergedRejectsConsumerEngineNamespace(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "collision", `
id: engine.prow.failure-evidence
triggers: ["failure"]
`)
	_, err := LoadMerged(dir, ProfileSelection{})
	if err == nil {
		t.Fatal("consumer collision with engine namespace was accepted")
	}
	if !strings.Contains(err.Error(), "reserved engine. namespace") {
		t.Fatalf("error = %q", err)
	}
}

func TestSetFromSourcesRejectsDuplicateID(t *testing.T) {
	skill := Skill{ID: "same", Triggers: []string{"x"}}
	if err := validateAndCompile(&skill); err != nil {
		t.Fatal(err)
	}
	_, err := setFromSources([]sourcedSkill{
		{skill: skill, source: "engine profile prow"},
		{skill: skill, source: "consumer skills"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate skill id") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestBuiltinTriggersMatchProceduralSignals(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id       string
		matching string
		notMatch string
	}{
		{
			id:       "engine.prow.failure-evidence",
			matching: "The test timed out while waiting for the condition.",
			notMatch: "The run completed successfully.",
		},
		{
			id:       "engine.prow.run-context",
			matching: "Cleanup and artifact collection happened after the test.",
			notMatch: "The webhook certificate is invalid.",
		},
		{
			id:       "engine.kubernetes.machine-node-providerid",
			matching: "The MachineDeployment timed out while the worker Node lacked providerID.",
			notMatch: "A webhook rejected an invalid certificate.",
		},
		{
			id:       "engine.kubernetes.pod-container-startup",
			matching: "The assigned Pod remained ContainerCreating after a CSI mount failure.",
			notMatch: "The source checkout used the wrong commit.",
		},
		{
			id:       "engine.kubernetes.cluster-control-plane-provisioning",
			matching: "The control plane Cluster timed out during provisioning.",
			notMatch: "The test assertion compared the wrong string.",
		},
		{
			id:       "engine.kubernetes.service-api-dns-connectivity",
			matching: "cloud-node-manager could not reach the Kubernetes API ClusterIP: connection refused.",
			notMatch: "The container image contains an invalid binary.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if !matchContains(set, tt.matching, tt.id) {
				t.Fatalf("%q did not match %q; got %v", tt.id, tt.matching, skillIDs(set.Match(tt.matching)))
			}
			if matchContains(set, tt.notMatch, tt.id) {
				t.Fatalf("%q unexpectedly matched %q", tt.id, tt.notMatch)
			}
		})
	}
}

func TestBuiltinEvidencePaths(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]map[string]string{
		"engine.prow.failure-evidence": {
			"build-log":     "build-log.txt",
			"junit-failure": "artifacts/junit.e2e_suite.1.xml",
		},
		"engine.prow.run-context": {
			"run-start":    "started.json",
			"run-finish":   "finished.json",
			"prow-context": "prowjob.json",
		},
		"engine.kubernetes.machine-node-providerid": {
			"machine-state":             "artifacts/clusters/bootstrap/resources/ns/Machine/machine.yaml",
			"node-state":                "artifacts/clusters/workload/nodes/node-1/node-describe.txt",
			"cloud-provider-controller": "artifacts/clusters/workload/kube-system/cloud-node-manager-node-1/cloud-node-manager.log",
			"kube-proxy":                "artifacts/clusters/workload/kube-system/kube-proxy-node-1/kube-proxy.log",
		},
		"engine.kubernetes.pod-container-startup": {
			"pod-state-events": "artifacts/clusters/workload/default/example-pod/pod-describe.txt",
			"kubelet":          "artifacts/clusters/workload/machines/node-1/kubelet.log",
			"device-plugin":    "artifacts/clusters/workload/kube-system/nvidia-device-plugin-node-1/plugin.log",
			"network-plugin":   "artifacts/clusters/workload/kube-system/calico-node-1/calico.log",
			"storage-plugin":   "artifacts/clusters/workload/kube-system/csi-node-1/plugin.log",
			"image-runtime":    "artifacts/clusters/workload/machines/node-1/containerd.log",
		},
		"engine.kubernetes.cluster-control-plane-provisioning": {
			"provisioning-object-state": "artifacts/clusters/bootstrap/resources/ns/KubeadmControlPlane/cp.yaml",
			"responsible-controller":    "artifacts/clusters/bootstrap/logs/capi-system/capi-controller-manager/pod/manager.log",
		},
		"engine.kubernetes.service-api-dns-connectivity": {
			"affected-client": "artifacts/clusters/workload/kube-system/cloud-node-manager-node-1/cloud-node-manager.log",
			"service-routing": "artifacts/clusters/workload/kube-system/kube-proxy-node-1/kube-proxy.log",
			"dns-resolution":  "artifacts/clusters/workload/nodes/node-1/resolv.conf",
		},
	}

	for skillID, groups := range paths {
		skill := findSkill(t, set, skillID)
		for groupID, artifactPath := range groups {
			t.Run(skillID+"/"+groupID, func(t *testing.T) {
				group := findGroup(t, skill, groupID)
				reads := map[string]bool{strings.ToLower(artifactPath): true}
				if !group.Satisfied(reads) {
					t.Fatalf("group %q did not match %q; patterns=%v", groupID, artifactPath, group.AnyOf)
				}
			})
		}
	}
}

func TestMachineEvidenceGroupsApplyByFailureClass(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	skill := findSkill(t, set, "engine.kubernetes.machine-node-providerid")
	cloud := findGroup(t, skill, "cloud-provider-controller")
	proxy := findGroup(t, skill, "kube-proxy")

	bootDraft := "The worker Machine timed out during bootstrap before the Node registered"
	if cloud.Applies(bootDraft) || proxy.Applies(bootDraft) {
		t.Fatalf("boot draft applicability: cloud=%v proxy=%v", cloud.Applies(bootDraft), proxy.Applies(bootDraft))
	}
	providerDraft := "The worker Node registered but providerID is missing and the cloud-provider taint remains"
	if !cloud.Applies(providerDraft) || proxy.Applies(providerDraft) {
		t.Fatalf("providerID draft applicability: cloud=%v proxy=%v", cloud.Applies(providerDraft), proxy.Applies(providerDraft))
	}
	serviceDraft := "cloud-node-manager cannot reach the Kubernetes API Service ClusterIP"
	if !cloud.Applies(serviceDraft) || !proxy.Applies(serviceDraft) {
		t.Fatalf("API reachability draft applicability: cloud=%v proxy=%v", cloud.Applies(serviceDraft), proxy.Applies(serviceDraft))
	}
}

func TestPodEvidenceGroupsApplyByFailureClass(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	skill := findSkill(t, set, "engine.kubernetes.pod-container-startup")
	kubelet := findGroup(t, skill, "kubelet")
	device := findGroup(t, skill, "device-plugin")
	network := findGroup(t, skill, "network-plugin")
	storage := findGroup(t, skill, "storage-plugin")
	image := findGroup(t, skill, "image-runtime")

	unscheduled := "Pod is Unschedulable after FailedScheduling due to insufficient CPU"
	for name, group := range map[string]EvidenceGroup{"kubelet": kubelet, "device": device, "network": network, "storage": storage, "image": image} {
		if group.Applies(unscheduled) {
			t.Errorf("unscheduled Pod unexpectedly required %s evidence", name)
		}
	}
	csiDraft := "Assigned Pod is ContainerCreating because the CSI volume mount failed"
	if !kubelet.Applies(csiDraft) || !storage.Applies(csiDraft) {
		t.Fatalf("CSI draft applicability: kubelet=%v storage=%v", kubelet.Applies(csiDraft), storage.Applies(csiDraft))
	}
	if device.Applies(csiDraft) || network.Applies(csiDraft) || image.Applies(csiDraft) {
		t.Fatalf("CSI draft required unrelated subsystem evidence")
	}
}

func TestConnectivityEvidenceGroupsApplyByFailureClass(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	skill := findSkill(t, set, "engine.kubernetes.service-api-dns-connectivity")
	service := findGroup(t, skill, "service-routing")
	dns := findGroup(t, skill, "dns-resolution")

	serviceDraft := "cloud-node-manager cannot reach the Kubernetes API ClusterIP: connection refused"
	if !service.Applies(serviceDraft) || dns.Applies(serviceDraft) {
		t.Fatalf("service draft applicability: service=%v dns=%v", service.Applies(serviceDraft), dns.Applies(serviceDraft))
	}
	dnsDraft := "API hostname lookup used a loopback DNS resolver that refused connections"
	if service.Applies(dnsDraft) || !dns.Applies(dnsDraft) {
		t.Fatalf("DNS draft applicability: service=%v dns=%v", service.Applies(dnsDraft), dns.Applies(dnsDraft))
	}
}

func TestBuiltinRecipesRemainProviderAndVersionNeutral(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, skill := range set.Skills() {
		if !strings.HasPrefix(skill.ID, "engine.") {
			continue
		}
		serialized := strings.ToLower(strings.Join(append(append([]string{}, skill.Triggers...), skill.Procedure), "\n"))
		for _, forbidden := range []string{`(?i)\bcapz\b`, `(?i)\bazure\b`, `(?i)\baso\b`, `(?i)\bflatcar\b`, `\b1\.\d+\b`} {
			if regexp.MustCompile(forbidden).MatchString(serialized) {
				t.Errorf("%s contains provider or version token matching %q", skill.ID, forbidden)
			}
		}
	}
}

func skillIDs(skills []Skill) []string {
	ids := make([]string, 0, len(skills))
	for _, skill := range skills {
		ids = append(ids, skill.ID)
	}
	return ids
}

func containsSkill(set *Set, id string) bool {
	return findSkillIndex(set, id) >= 0
}

func matchContains(set *Set, text, id string) bool {
	for _, skill := range set.Match(text) {
		if skill.ID == id {
			return true
		}
	}
	return false
}

func findSkillIndex(set *Set, id string) int {
	for i, skill := range set.Skills() {
		if skill.ID == id {
			return i
		}
	}
	return -1
}

func findSkill(t *testing.T, set *Set, id string) Skill {
	t.Helper()
	index := findSkillIndex(set, id)
	if index < 0 {
		ids := skillIDs(set.Skills())
		sort.Strings(ids)
		t.Fatalf("skill %q not found in %v", id, ids)
	}
	return set.Skills()[index]
}

func findGroup(t *testing.T, skill Skill, id string) EvidenceGroup {
	t.Helper()
	for _, group := range skill.RequiredEvidence {
		if group.ID == id {
			return group
		}
	}
	t.Fatalf("evidence group %q not found in %s", id, skill.ID)
	return EvidenceGroup{}
}

func TestLoadForToolsKeepsFilesystemOnlyConsumersOutOfKubernetesProfile(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "consumer", `
id: consumer-filesystem
triggers: ["filesystem"]
`)
	set, selection, err := LoadForTools(dir, []string{"filesystem"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Kubernetes {
		t.Fatal("filesystem-only tools selected the Kubernetes profile")
	}
	if !containsSkill(set, "engine.prow.failure-evidence") || !containsSkill(set, "consumer-filesystem") {
		t.Fatalf("filesystem-only merged IDs = %v", skillIDs(set.Skills()))
	}
	for _, skill := range set.Skills() {
		if strings.HasPrefix(skill.ID, "engine.kubernetes.") {
			t.Fatalf("filesystem-only set contains %q", skill.ID)
		}
	}
}
