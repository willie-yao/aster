package benchmarks

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const shadowBenchmarkKindContext = "kind-prow-ai-shadow-bench"

// shadowBenchmarkClusterIdentity pins the exact disposable cluster a private
// comparison benchmark is allowed to run against.
type shadowBenchmarkClusterIdentity struct {
	Server                   string
	CertificateAuthorityData string
}

func verifyShadowBenchmarkCluster(t *testing.T, contextName string) {
	t.Helper()
	clustersOutput, err := exec.Command("kind", "get", "clusters").CombinedOutput()
	if err != nil {
		t.Fatal("kind cluster discovery failed")
	}
	clusterName := strings.TrimPrefix(shadowBenchmarkKindContext, "kind-")
	kindKubeconfig, err := exec.Command("kind", "get", "kubeconfig", "--name", clusterName).Output()
	if err != nil {
		t.Fatal("kind kubeconfig discovery failed")
	}
	kindPath := filepath.Join(t.TempDir(), "kind-kubeconfig")
	if err := os.WriteFile(kindPath, kindKubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	selected := kubectlConfigIdentity(t, "--context", contextName)
	expected := kubectlConfigIdentity(t, "--kubeconfig", kindPath)
	if err := admitShadowBenchmarkCluster(contextName, strings.Fields(string(clustersOutput)), selected, expected); err != nil {
		t.Fatal(err)
	}
}

func kubectlConfigIdentity(t *testing.T, selectorFlag, selectorValue string) shadowBenchmarkClusterIdentity {
	t.Helper()
	output, err := exec.Command("kubectl", "config", "view", "--raw", "--minify", selectorFlag, selectorValue, "-o", "json").CombinedOutput()
	if err != nil {
		t.Fatal("kubectl context identity lookup failed")
	}
	var view struct {
		Clusters []struct {
			Cluster struct {
				Server                   string `json:"server"`
				CertificateAuthorityData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(output, &view); err != nil || len(view.Clusters) != 1 {
		t.Fatal("kubectl context identity is malformed")
	}
	return shadowBenchmarkClusterIdentity{
		Server:                   strings.TrimSpace(view.Clusters[0].Cluster.Server),
		CertificateAuthorityData: strings.TrimSpace(view.Clusters[0].Cluster.CertificateAuthorityData),
	}
}

func admitShadowBenchmarkCluster(contextName string, clusters []string, selected, expected shadowBenchmarkClusterIdentity) error {
	if strings.TrimSpace(contextName) != shadowBenchmarkKindContext {
		return fmt.Errorf("shadow benchmark requires disposable context %q", shadowBenchmarkKindContext)
	}
	clusterName := strings.TrimPrefix(shadowBenchmarkKindContext, "kind-")
	if !slices.Contains(clusters, clusterName) {
		return fmt.Errorf("shadow benchmark kind cluster %q is not present", clusterName)
	}
	if selected.Server == "" || selected.CertificateAuthorityData == "" || selected != expected {
		return fmt.Errorf("shadow benchmark context does not target the disposable kind cluster")
	}
	return nil
}

func TestAdmitShadowBenchmarkCluster(t *testing.T) {
	identity := shadowBenchmarkClusterIdentity{Server: "https://127.0.0.1:6443", CertificateAuthorityData: "ca"}
	if err := admitShadowBenchmarkCluster(shadowBenchmarkKindContext, []string{"prow-ai-shadow-bench"}, identity, identity); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		context  string
		clusters []string
		selected shadowBenchmarkClusterIdentity
		expected shadowBenchmarkClusterIdentity
	}{
		{context: "production", clusters: []string{"prow-ai-shadow-bench"}, selected: identity, expected: identity},
		{context: shadowBenchmarkKindContext, clusters: []string{"other"}, selected: identity, expected: identity},
		{context: shadowBenchmarkKindContext, clusters: []string{"prow-ai-shadow-bench"}, selected: shadowBenchmarkClusterIdentity{Server: "https://other", CertificateAuthorityData: "ca"}, expected: identity},
		{context: shadowBenchmarkKindContext, clusters: []string{"prow-ai-shadow-bench"}, selected: shadowBenchmarkClusterIdentity{Server: identity.Server, CertificateAuthorityData: "other-ca"}, expected: identity},
	} {
		if err := admitShadowBenchmarkCluster(test.context, test.clusters, test.selected, test.expected); err == nil {
			t.Fatalf("context=%q clusters=%v was admitted", test.context, test.clusters)
		}
	}
}
