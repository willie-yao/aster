package k8s

import (
	"testing"
)

func TestMapTestToClusterSingleCluster(t *testing.T) {
	clusters := []Cluster{{Name: "capz-e2e-abc123-ha"}}
	got := MapTestToCluster("any test name", clusters)
	if got == nil || got.Name != "capz-e2e-abc123-ha" {
		t.Errorf("single-cluster fallback failed, got %v", got)
	}
}

func TestMapTestToClusterEmpty(t *testing.T) {
	if got := MapTestToCluster("any test", nil); got != nil {
		t.Errorf("expected nil for empty clusters, got %v", got)
	}
}

func TestMapTestToClusterFlavorSubstring(t *testing.T) {
	clusters := []Cluster{
		{Name: "quick-start-bxqxxs"},
		{Name: "kcp-adoption-xyz789"},
		{Name: "self-hosted-def456"},
	}
	cases := []struct {
		test, want string
	}{
		{"Workload cluster creation Should successfully create with quick-start", "quick-start-bxqxxs"},
		{"KCP Adoption tests Should successfully adopt control plane", "kcp-adoption-xyz789"},
		{"Self-hosted Should successfully pivot resources to self-hosted cluster", "self-hosted-def456"},
	}
	for _, c := range cases {
		t.Run(c.test, func(t *testing.T) {
			got := MapTestToCluster(c.test, clusters)
			if got == nil || got.Name != c.want {
				t.Errorf("MapTestToCluster(%q) = %v, want %s", c.test, got, c.want)
			}
		})
	}
}

func TestMapTestToClusterCAPZRulesTable(t *testing.T) {
	clusters := []Cluster{
		{Name: "capz-e2e-abc123-ha"},
		{Name: "capz-e2e-abc123-ipv6"},
		{Name: "capz-e2e-abc123-windows"},
		{Name: "capz-e2e-abc123-machine-pool"},
		{Name: "capz-e2e-abc123-aks"},
		{Name: "capz-e2e-abc123-dual-stack"},
		{Name: "capz-e2e-abc123-azl3"},
	}
	cases := []struct {
		test, want string
	}{
		{"[It] IPv6 networking works", "capz-e2e-abc123-ipv6"},
		{"[It] Windows nodes join cluster", "capz-e2e-abc123-windows"},
		{"[It] VMSS scale set works", "capz-e2e-abc123-machine-pool"},
		{"[It] AKS managed cluster", "capz-e2e-abc123-aks"},
		{"[It] Dual-stack networking", "capz-e2e-abc123-dual-stack"},
		{"[It] Azure Linux 3 node pools", "capz-e2e-abc123-azl3"},
	}
	for _, c := range cases {
		t.Run(c.test, func(t *testing.T) {
			got := MapTestToCluster(c.test, clusters)
			if got == nil || got.Name != c.want {
				t.Errorf("MapTestToCluster(%q) = %v, want %s", c.test, got, c.want)
			}
		})
	}
}

func TestMapTestToClusterNoMatch(t *testing.T) {
	clusters := []Cluster{
		{Name: "capz-e2e-abc123-ha"},
		{Name: "capz-e2e-abc123-ipv6"},
	}
	if got := MapTestToCluster("some completely unrelated test", clusters); got != nil {
		t.Errorf("expected nil for unmatched test, got %v", got)
	}
}

func TestMapTestToClusterPrefersLongestFlavor(t *testing.T) {
	clusters := []Cluster{
		{Name: "kcp-adoption-xyz789"},
		{Name: "self-hosted-kcp-adoption-abc012"},
	}
	got := MapTestToCluster("Self-hosted KCP adoption Should pivot then adopt", clusters)
	if got == nil || got.Name != "self-hosted-kcp-adoption-abc012" {
		t.Errorf("longest flavor should win, got %v", got)
	}
}

func TestNormalizeForMatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Quick-Start", "quick-start"},
		{"  [It] Foo bar  ", "it-foo-bar"},
		{"KCP_Adoption!!", "kcp-adoption"},
		{"capz-e2e-abc123-azl3", "capz-e2e-abc123-azl3"},
		{"", ""},
		{"---", ""},
	}
	for _, c := range cases {
		if got := normalizeForMatch(c.in); got != c.want {
			t.Errorf("normalizeForMatch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClusterFlavorKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"quick-start-bxqxxs", "quick-start"},
		{"kcp-adoption-xyz789", "kcp-adoption"},
		// CAPZ-style: trailing -ha is only 2 chars, not stripped.
		{"capz-e2e-abc123-ha", "capz-e2e-abc123-ha"},
		// CAPZ-style: -windows is 7 chars and IS stripped; the leftover
		// "capz-e2e-abc123" won't appear in human-readable test names so
		// the matcher still doesn't false-positive.
		{"capz-e2e-abc123-windows", "capz-e2e-abc123"},
		{"ab", ""},
		{"bxqxxs", "bxqxxs"},
	}
	for _, c := range cases {
		if got := clusterFlavorKey(c.in); got != c.want {
			t.Errorf("clusterFlavorKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
