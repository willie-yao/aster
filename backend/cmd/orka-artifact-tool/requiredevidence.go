package main

import (
	"log"
	"net/http"
	"strings"
)

type requiredEvidenceRule struct {
	Class    string
	Keywords []string
	MustRead []string
	Notes    string
}

type requiredEvidenceResponse struct {
	Signal       string   `json:"signal"`
	MatchedClass string   `json:"matched_class"`
	MustRead     []string `json:"must_read"`
	Notes        string   `json:"notes"`
}

var requiredEvidenceRules = []requiredEvidenceRule{
	{
		Class: "control_plane_init",
		Keywords: []string{
			"control_plane_init", "control plane", "kubeadm init", "apiserver", "api server", "etcd",
		},
		MustRead: []string{
			"artifacts/clusters/{cluster}/machines/{cp}/cloud-init-output.log",
			"artifacts/clusters/{cluster}/machines/{cp}/kubelet.log",
			"build-log.txt",
		},
		Notes: "kubeadm init runs only on the first CP machine",
	},
	{
		Class: "worker_bootstrap",
		Keywords: []string{
			"worker_bootstrap", "worker", "machinedeployment", "machine deployment", "kubelet", "provisioned",
		},
		MustRead: []string{
			"artifacts/clusters/{cluster}/machines/{worker}/cloud-init-output.log",
			"artifacts/clusters/{cluster}/machines/{worker}/kubelet.log",
			"artifacts/clusters/{cluster}/machines/{worker}/boot.log",
		},
		Notes: "provisioned but not running usually means kubelet never registered",
	},
	{
		Class: "cni",
		Keywords: []string{
			"cni", "calico", "networking", "containercreating", "container creating", "clusterip", "kube-proxy",
		},
		MustRead: []string{
			"artifacts/clusters/{bootstrap-cluster}/logs/kube-system/calico-node/{pod}/calico-node.log",
			"artifacts/clusters/{workload-cluster}/logs/kube-system/calico-node/{pod}/calico-node.log",
			"artifacts/clusters/{cluster}/logs/kube-system/kube-proxy/{pod}/kube-proxy.log",
			"artifacts/clusters/{cluster}/Cluster/",
		},
		Notes: "check cni=calico label + kube-proxy first",
	},
	{
		Class: "cloud_provider",
		Keywords: []string{
			"cloud_provider", "cloud-provider", "cloud provider", "providerid", "provider id", "ccm", "cnm", "cloud-node-manager",
		},
		MustRead: []string{
			"artifacts/clusters/{cluster}/logs/kube-system/cloud-controller-manager/{pod}/cloud-controller-manager.log",
			"artifacts/clusters/{cluster}/logs/kube-system/cloud-node-manager/{pod}/cloud-node-manager.log",
		},
		Notes: "usually cascades from kube-proxy",
	},
	{
		Class: "security_group",
		Keywords: []string{
			"security_group", "security group", "network security group", "nsg", "securityrule", "security rule",
		},
		MustRead: []string{
			"artifacts/clusters/{cluster}/azure-activity-logs/{cluster}.log",
		},
		Notes: "correlate ARM operation timestamps vs the test's Eventually timeout",
	},
	{
		Class: "upgrade",
		Keywords: []string{
			"upgrade", "clusterctl", "apiversion", "api version", "contract", "provider deployment",
		},
		MustRead: []string{
			"artifacts/clusters/{management-cluster}/logs/capi-system/capi-controller-manager/{pod}/manager.log",
			"artifacts/clusters/{management-cluster}/logs/capz-system/capz-controller-manager/{pod}/manager.log",
			"artifacts/clusters/{management-cluster}/Deployment/",
		},
		Notes: "check management-cluster controller logs and provider deployment status",
	},
	{
		Class: "vm_bootstrap",
		Keywords: []string{
			"vm_bootstrap", "cloud-init", "cloud init", "vm extension", "bootstrapping", "capz.linux.bootstrapping",
		},
		MustRead: []string{
			"artifacts/clusters/{cluster}/machines/{machine}/cloud-init-output.log",
			"artifacts/clusters/{cluster}/machines/{machine}/boot.log",
			"artifacts/clusters/{cluster}/azure-activity-logs/{cluster}.log",
		},
		Notes: "a VM extension error means cloud-init failed; debug cloud-init, not the extension",
	},
	{
		Class: "image_pull",
		Keywords: []string{
			"image_pull", "imagepull", "image pull", "imagepullbackoff", "errimagepull", "registry", "rate limiting",
		},
		MustRead: []string{
			"build-log.txt",
			"artifacts/clusters/{cluster}/Pod/",
			"artifacts/clusters/{cluster}/Event/",
		},
		Notes: "capture the exact image ref and verify whether the tag exists, the registry is reachable, or rate limiting occurred",
	},
}

var requiredEvidenceFallback = requiredEvidenceRule{
	Class: "general",
	MustRead: []string{
		"build-log.txt",
		"artifacts/clusters/{cluster}/machines/{machine}/kubelet.log",
		"artifacts/clusters/{cluster}/machines/{machine}/cloud-init-output.log",
		"artifacts/clusters/{cluster}/azure-activity-logs/{cluster}.log",
		"artifacts/clusters/{cluster}/Machine/",
		"artifacts/clusters/{cluster}/AzureMachine/",
		"artifacts/clusters/{cluster}/MachinePool/",
		"artifacts/clusters/{cluster}/AzureMachinePool/",
	},
	Notes: "follow the CAPZ triage order before making a root-cause claim",
}

func init() {
	registerQTool("/tool/required_evidence", requiredEvidence)
}

func requiredEvidence(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Signal string `json:"signal"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	signal := strings.TrimSpace(args.Signal)
	if signal == "" {
		http.Error(w, "signal is required", http.StatusBadRequest)
		return
	}

	rule := requiredEvidenceFor(signal)
	log.Printf("📋 required_evidence signal=%q class=%s", signal, rule.Class)
	writeJSON(w, requiredEvidenceResponse{
		Signal:       signal,
		MatchedClass: rule.Class,
		MustRead:     rule.MustRead,
		Notes:        rule.Notes,
	})
}

func requiredEvidenceFor(signal string) requiredEvidenceRule {
	foldedSignal := strings.ToLower(signal)
	for _, rule := range requiredEvidenceRules {
		for _, keyword := range rule.Keywords {
			if strings.Contains(foldedSignal, strings.ToLower(keyword)) {
				return rule
			}
		}
	}
	return requiredEvidenceFallback
}
