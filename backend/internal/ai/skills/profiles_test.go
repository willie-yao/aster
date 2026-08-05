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
			id:       "engine.prow.job-config",
			matching: "The value for configuration variables IMAGE_TAG and ARTIFACT_BUCKET is not set.",
			notMatch: "The application failed because a ConfigMap data key was missing.",
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
		"engine.prow.job-config": {
			"effective-job-config": "prowjob.json",
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
	for _, authorizationDraft := range []string{
		"providerID is missing, but the Kubernetes API returned an authorization error",
		"providerID is missing, but cloud-node-manager's Kubernetes API request failed with an authorization error",
	} {
		if proxy.Applies(authorizationDraft) {
			t.Errorf("authorization error unexpectedly required kube-proxy evidence: %q", authorizationDraft)
		}
	}
	connectionOrderDraft := "providerID is missing because cloud-node-manager's connection to the Kubernetes API Service timed out"
	if !proxy.Applies(connectionOrderDraft) {
		t.Fatal("operation-first API connection did not require kube-proxy evidence")
	}
	synchronizationDraft := "providerID is missing and kube-proxy never synchronized"
	if !proxy.Applies(synchronizationDraft) {
		t.Fatal("kube-proxy synchronization failure did not require kube-proxy evidence")
	}
	connectivityDraft := "providerID is missing and Kubernetes API Service connectivity failed"
	if !proxy.Applies(connectivityDraft) {
		t.Fatal("API Service connectivity failure did not require kube-proxy evidence")
	}
	kubeProxyRequest := "providerID is missing and kube-proxy request timed out"
	if !proxy.Applies(kubeProxyRequest) {
		t.Fatal("kube-proxy request timeout did not require kube-proxy evidence")
	}
	kubeProxyAuthorization := "providerID is missing and kube-proxy request failed with an authorization error"
	if proxy.Applies(kubeProxyAuthorization) {
		t.Fatal("kube-proxy authorization error unexpectedly required kube-proxy transport evidence")
	}
	passiveReachability := "providerID is missing because cloud-node-manager reports that the Kubernetes API Service could not be reached"
	if !proxy.Applies(passiveReachability) {
		t.Fatal("passive API Service reachability failure did not require kube-proxy evidence")
	}
	failureBeforeConnection := "providerID is missing because cloud-node-manager had a failed connection to the Kubernetes API Service"
	if !proxy.Applies(failureBeforeConnection) {
		t.Fatal("failure-before-connection wording did not require kube-proxy evidence")
	}
}

func TestProviderIDDiagnosisMatchesOnlyRelevantBuiltinRecipes(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	draft := "The worker Node exists but has no providerID and retains the cloud-provider uninitialized taint. " +
		"The MachineDeployment controller timed out waiting for that providerID. " +
		"The cloud-node-manager controller entered CrashLoopBackOff because it could not reach the Kubernetes API Service ClusterIP. " +
		"kube-proxy never synchronized because the API hostname lookup through the DNS resolver was refused."

	for _, id := range []string{
		"engine.kubernetes.machine-node-providerid",
		"engine.kubernetes.service-api-dns-connectivity",
	} {
		if !matchContains(set, draft, id) {
			t.Errorf("providerID draft did not match %q; got %v", id, skillIDs(set.Match(draft)))
		}
	}
	for _, id := range []string{
		"engine.kubernetes.pod-container-startup",
		"engine.kubernetes.cluster-control-plane-provisioning",
		"engine.prow.run-context",
	} {
		if matchContains(set, draft, id) {
			t.Errorf("providerID draft unexpectedly matched %q", id)
		}
	}

	provider := findSkill(t, set, "engine.kubernetes.machine-node-providerid")
	if got, want := applicableEvidenceIDs(provider, draft), []string{"machine-state", "node-state", "cloud-provider-controller", "kube-proxy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("providerID evidence = %v, want %v", got, want)
	}
	connectivity := findSkill(t, set, "engine.kubernetes.service-api-dns-connectivity")
	if got, want := applicableEvidenceIDs(connectivity, draft), []string{"affected-client", "service-routing", "dns-resolution"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("connectivity evidence = %v, want %v", got, want)
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

	crashLoopDraft := "The application Pod startup failed after its container entered CrashLoopBackOff."
	if !matchContains(set, crashLoopDraft, skill.ID) {
		t.Fatalf("Pod startup draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, crashLoopDraft), []string{"pod-state-events", "kubelet"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Pod startup evidence = %v, want %v", got, want)
	}
	directCrashLoopDraft := "The container entered CrashLoopBackOff."
	if !matchContains(set, directCrashLoopDraft, skill.ID) {
		t.Fatalf("direct CrashLoopBackOff draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, directCrashLoopDraft), []string{"pod-state-events", "kubelet"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("direct CrashLoopBackOff evidence = %v, want %v", got, want)
	}
}

func TestClusterProvisioningDiagnosisRequiresResponsibleController(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	draft := "Provisioning failed for the workload Machine while its infrastructure controller reconciliation stalled."
	skill := findSkill(t, set, "engine.kubernetes.cluster-control-plane-provisioning")
	if !matchContains(set, draft, skill.ID) {
		t.Fatalf("cluster provisioning draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, draft), []string{"provisioning-object-state", "responsible-controller"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cluster provisioning evidence = %v, want %v", got, want)
	}
	operationFirst := "Provisioning of the workload Machine timed out."
	if !matchContains(set, operationFirst, skill.ID) {
		t.Fatalf("operation-first provisioning draft did not match %q", skill.ID)
	}
	operationFailureFirst := "Provisioning failed for the workload Machine."
	if !matchContains(set, operationFailureFirst, skill.ID) {
		t.Fatalf("operation-failure provisioning draft did not match %q", skill.ID)
	}
	for _, draft := range []string{
		"The workload Machine provisioning failed.",
		"The workload Machine failed during provisioning.",
		"Provisioning of the workload Machine failed.",
		"Provisioning failed for the workload Machine.",
		"Failed provisioning of the workload Machine.",
		"Failed workload Machine provisioning.",
		"The workload Machine could not be provisioned.",
		"Unable to provision the workload Machine.",
	} {
		if !matchContains(set, draft, skill.ID) {
			t.Errorf("provisioning permutation did not match %q: %q", skill.ID, draft)
		}
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
	healthyDraft := "cloud-node-manager can reach the Kubernetes API Service; providerID reconciliation is blocked elsewhere"
	if matchContains(set, healthyDraft, skill.ID) || service.Applies(healthyDraft) || dns.Applies(healthyDraft) {
		t.Fatalf("healthy API draft matched connectivity requirements: match=%v service=%v dns=%v",
			matchContains(set, healthyDraft, skill.ID), service.Applies(healthyDraft), dns.Applies(healthyDraft))
	}
	for _, authorizationDraft := range []string{
		"The Kubernetes Service returned an authorization error after accepting the request",
		"The Kubernetes API request failed with an authorization error",
	} {
		if matchContains(set, authorizationDraft, skill.ID) || service.Applies(authorizationDraft) || dns.Applies(authorizationDraft) {
			t.Errorf("authorization error matched connectivity requirements: draft=%q match=%v service=%v dns=%v",
				authorizationDraft, matchContains(set, authorizationDraft, skill.ID), service.Applies(authorizationDraft), dns.Applies(authorizationDraft))
		}
	}
	connectionFailure := "The Kubernetes API Service connection failed before the request reached the server"
	if !matchContains(set, connectionFailure, skill.ID) || !service.Applies(connectionFailure) {
		t.Fatalf("connection failure did not match connectivity requirements: match=%v service=%v",
			matchContains(set, connectionFailure, skill.ID), service.Applies(connectionFailure))
	}
	for _, draft := range []string{
		"The Kubernetes API Service connection failed",
		"The Kubernetes API Service failed connection",
		"The connection to the Kubernetes API Service failed",
		"The connection failed to the Kubernetes API Service",
		"A failed connection to the Kubernetes API Service blocked startup",
		"The failed Kubernetes API Service connection blocked startup",
	} {
		if !matchContains(set, draft, skill.ID) || !service.Applies(draft) {
			t.Errorf("connectivity permutation did not match service evidence: draft=%q match=%v service=%v",
				draft, matchContains(set, draft, skill.ID), service.Applies(draft))
		}
	}
	operationFirst := "The connection to the Kubernetes API Service timed out"
	if !matchContains(set, operationFirst, skill.ID) || !service.Applies(operationFirst) {
		t.Fatalf("operation-first connection failure did not match connectivity requirements: match=%v service=%v",
			matchContains(set, operationFirst, skill.ID), service.Applies(operationFirst))
	}
	requestTimeout := "The Kubernetes API request timed out before receiving a response"
	if !matchContains(set, requestTimeout, skill.ID) || !service.Applies(requestTimeout) {
		t.Fatalf("request timeout did not match connectivity requirements: match=%v service=%v",
			matchContains(set, requestTimeout, skill.ID), service.Applies(requestTimeout))
	}
	synchronizationFailure := "kube-proxy never synchronized"
	if !matchContains(set, synchronizationFailure, skill.ID) || !service.Applies(synchronizationFailure) {
		t.Fatalf("kube-proxy synchronization failure did not match connectivity requirements: match=%v service=%v",
			matchContains(set, synchronizationFailure, skill.ID), service.Applies(synchronizationFailure))
	}
	dnsNegative := "DNS never resolved the Kubernetes API hostname"
	if !matchContains(set, dnsNegative, skill.ID) || service.Applies(dnsNegative) || !dns.Applies(dnsNegative) {
		t.Fatalf("negative DNS resolution did not match DNS-only evidence: match=%v service=%v dns=%v",
			matchContains(set, dnsNegative, skill.ID), service.Applies(dnsNegative), dns.Applies(dnsNegative))
	}
	connectivityFailure := "Kubernetes API Service connectivity failed"
	if !matchContains(set, connectivityFailure, skill.ID) || !service.Applies(connectivityFailure) {
		t.Fatalf("Service connectivity failure did not match connectivity requirements: match=%v service=%v",
			matchContains(set, connectivityFailure, skill.ID), service.Applies(connectivityFailure))
	}
	kubeProxyRequest := "kube-proxy request timed out"
	if !matchContains(set, kubeProxyRequest, skill.ID) || !service.Applies(kubeProxyRequest) {
		t.Fatalf("kube-proxy request timeout did not match connectivity requirements: match=%v service=%v",
			matchContains(set, kubeProxyRequest, skill.ID), service.Applies(kubeProxyRequest))
	}
	kubeProxyAuthorization := "kube-proxy request failed with an authorization error"
	if matchContains(set, kubeProxyAuthorization, skill.ID) || service.Applies(kubeProxyAuthorization) {
		t.Fatalf("kube-proxy authorization error matched transport requirements: match=%v service=%v",
			matchContains(set, kubeProxyAuthorization, skill.ID), service.Applies(kubeProxyAuthorization))
	}
	passiveReachability := "The Kubernetes API Service could not be reached"
	if !matchContains(set, passiveReachability, skill.ID) || !service.Applies(passiveReachability) {
		t.Fatalf("passive Service reachability did not match connectivity requirements: match=%v service=%v",
			matchContains(set, passiveReachability, skill.ID), service.Applies(passiveReachability))
	}
	passiveDNS := "The Kubernetes API hostname could not be resolved"
	if !matchContains(set, passiveDNS, skill.ID) || service.Applies(passiveDNS) || !dns.Applies(passiveDNS) {
		t.Fatalf("passive DNS failure did not match DNS-only evidence: match=%v service=%v dns=%v",
			matchContains(set, passiveDNS, skill.ID), service.Applies(passiveDNS), dns.Applies(passiveDNS))
	}
	for _, draft := range []string{
		"The Kubernetes API hostname lookup failed",
		"The Kubernetes API hostname failed lookup",
		"Lookup for the Kubernetes API hostname failed",
		"Lookup failed for the Kubernetes API hostname",
		"A failed lookup of the Kubernetes API hostname blocked startup",
		"The failed Kubernetes API hostname lookup blocked startup",
	} {
		if !matchContains(set, draft, skill.ID) || service.Applies(draft) || !dns.Applies(draft) {
			t.Errorf("DNS permutation did not match DNS-only evidence: draft=%q match=%v service=%v dns=%v",
				draft, matchContains(set, draft, skill.ID), service.Applies(draft), dns.Applies(draft))
		}
	}
	dnsDraft := "API hostname lookup used a loopback DNS resolver that refused connections"
	if service.Applies(dnsDraft) || !dns.Applies(dnsDraft) {
		t.Fatalf("DNS draft applicability: service=%v dns=%v", service.Applies(dnsDraft), dns.Applies(dnsDraft))
	}
}

func TestProwRunContextEvidenceAppliesByClaim(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	skill := findSkill(t, set, "engine.prow.run-context")

	componentLifecycle := "The cloud-node-manager container started and finished before controller cleanup retried."
	if matchContains(set, componentLifecycle, skill.ID) {
		t.Fatalf("component lifecycle unexpectedly matched %q", skill.ID)
	}
	workloadCleanup := "The Kubernetes Job failed during controller cleanup."
	if matchContains(set, workloadCleanup, skill.ID) {
		t.Fatalf("workload cleanup unexpectedly matched %q", skill.ID)
	}
	unrelatedCommit := "Artifact collection failed; the database transaction commit was rolled back."
	if !matchContains(set, unrelatedCommit, skill.ID) {
		t.Fatalf("artifact collection draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, unrelatedCommit), []string{"run-start", "run-finish"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("artifact collection evidence = %v, want %v", got, want)
	}

	runLifecycle := "The Prow run finished during teardown, so the diagnosis depends on cleanup timing and duration."
	if !matchContains(set, runLifecycle, skill.ID) {
		t.Fatalf("run lifecycle draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, runLifecycle), []string{"run-start", "run-finish"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run lifecycle evidence = %v, want %v", got, want)
	}

	prowContext := "The Prow job checked out the wrong commit, so the tested revision differs from the intended one."
	if !matchContains(set, prowContext, skill.ID) {
		t.Fatalf("Prow context draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, prowContext), []string{"prow-context"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Prow context evidence = %v, want %v", got, want)
	}
	prowScheduling := "The Prow scheduler selected the wrong pod."
	if !matchContains(set, prowScheduling, skill.ID) {
		t.Fatalf("Prow scheduling draft did not match %q", skill.ID)
	}
	if got, want := applicableEvidenceIDs(skill, prowScheduling), []string{"prow-context"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Prow scheduling evidence = %v, want %v", got, want)
	}
}

func TestProwJobConfigPlansHistoricalSelectionFailure(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{})
	if err != nil {
		t.Fatal(err)
	}
	signal := `Failed test: [It] Workload cluster creation Creating a self-managed cluster and deploying an optional addon [Addon] Creates a workload
Failure message:
Failed to run cluster configuration
Unexpected error: value for variables [ARTIFACT_BUCKET, IMAGE_TAG] is not set. Please set the value using environment variables or the config file`
	if !matchContains(set, signal, "engine.prow.job-config") {
		t.Fatalf("historical selection failure did not match job config recipe: %v", skillIDs(set.Match(signal)))
	}

	planned := set.Plan(signal, []string{
		"build-log.txt",
		"prowjob.json",
		"artifacts/junit.e2e_suite.1.xml",
	}, 4)
	for _, skill := range planned {
		if skill.ID != "engine.prow.job-config" {
			continue
		}
		if len(skill.RequiredEvidence) != 1 {
			t.Fatalf("required evidence = %+v", skill.RequiredEvidence)
		}
		group := skill.RequiredEvidence[0]
		if group.ID != "effective-job-config" || !reflect.DeepEqual(group.CandidatePaths, []string{"prowjob.json"}) {
			t.Fatalf("job config plan = %+v", group)
		}
		if !strings.Contains(skill.Procedure, "Suggest skipping a test only") || !strings.Contains(skill.Procedure, "authoritative configuration that executed") {
			t.Fatalf("job config procedure lacks ownership or skip guard: %q", skill.Procedure)
		}
		return
	}
	t.Fatalf("job config recipe missing from plan: %+v", planned)
}

func TestProwJobConfigMatchesVersionSelectionFailures(t *testing.T) {
	set, err := LoadMerged(t.TempDir(), ProfileSelection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{
		"The API Version Upgrade used a Kubernetes release that is no longer available.",
		"The stale node image version was not found during the upgrade.",
		"GINKGO_SKIP selected the wrong test suite and the test failed.",
	} {
		if !matchContains(set, failure, "engine.prow.job-config") {
			t.Errorf("job config recipe did not match %q", failure)
		}
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
		parts := append([]string{skill.ID, skill.Name, skill.Description}, skill.Triggers...)
		parts = append(parts, skill.Procedure)
		for _, group := range skill.RequiredEvidence {
			parts = append(parts, group.ID, group.Description)
			parts = append(parts, group.When...)
			parts = append(parts, group.AnyOf...)
		}
		serialized := strings.ToLower(strings.Join(parts, "\n"))
		for _, forbidden := range []string{`(?i)\bcapz\b`, `(?i)\bazure\b`, `(?i)\baso\b`, `(?i)\bflatcar\b`, `\b1\.\d+\b`} {
			if regexp.MustCompile(forbidden).MatchString(serialized) {
				t.Errorf("%s contains provider or version token matching %q", skill.ID, forbidden)
			}
		}
	}
}

func applicableEvidenceIDs(skill Skill, draft string) []string {
	var ids []string
	for _, group := range skill.RequiredEvidence {
		if group.Applies(draft) {
			ids = append(ids, group.ID)
		}
	}
	return ids
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

func TestLoadMergedReportsBundleMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "consumer.yaml"), []byte("id: consumer.recipe\ntriggers: [boom]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, selection, err := LoadForTools(dir, []string{"filesystem"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Kubernetes || set.EngineCount() != 3 || set.ConsumerCount() != 1 || !set.ConsumerBundlePresent() {
		t.Fatalf("metadata: selection=%+v engine=%d consumer=%d present=%t", selection, set.EngineCount(), set.ConsumerCount(), set.ConsumerBundlePresent())
	}
	ids := set.IDs()
	if len(ids) != 4 || ids[0] == "" || ids[1] == "" || ids[2] == "" || ids[3] == "" {
		t.Fatalf("ids = %v", ids)
	}
	ids[0] = "mutated"
	if set.IDs()[0] == "mutated" {
		t.Fatal("IDs returned an aliased slice")
	}
}

func TestProwBuildPlanCoversCompleteSparseArtifactTree(t *testing.T) {
	set, _, err := LoadForTools(t.TempDir(), []string{"filesystem"})
	if err != nil {
		t.Fatal(err)
	}
	signal := "Failed Prow build: Prow job execution\nFailure message:\nThe Prow job failed without reporting a failed JUnit test case. Investigate build-log.txt for the root cause."
	paths := []string{
		"build-log.txt", "clone-log.txt", "clone-records.json", "finished.json",
		"podinfo.json", "prowjob.json", "sidecar-logs.json", "started.json",
	}
	plan := set.Plan(signal, paths, 4)
	coverage := set.PlanCoverageWithContent(signal, plan, map[string]bool{
		"build-log.txt": true,
		"podinfo.json":  true,
	}, nil)
	if coverage.Applicable != 3 || coverage.Satisfied != 2 || coverage.Unavailable != 1 || coverage.Unmet != 0 || !coverage.Covered() {
		t.Fatalf("coverage = %+v, plan = %+v", coverage, plan)
	}
}
