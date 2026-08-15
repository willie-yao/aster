package k8s

import (
	"regexp"
	"strings"
)

// MapTestToCluster picks the cluster most likely to own a given test name.
// Single-cluster builds always return that cluster. Otherwise the
// provider-agnostic flavor-substring matcher tries first; on miss, the
// CAPZ-flavored rules table fills in. Returns nil when no cluster matches.
//
// The CAPZ-flavored rules table preserves current CAPZ and CAPI core
// test-to-cluster mapping as the engine default.
func MapTestToCluster(testName string, clusters []Cluster) *Cluster {
	if len(clusters) == 0 {
		return nil
	}
	if len(clusters) == 1 {
		return &clusters[0]
	}

	if c := matchByFlavorSubstring(testName, clusters); c != nil {
		return c
	}

	lower := strings.ToLower(testName)

	type ruleMatch struct {
		ruleIdx int
		cluster *Cluster
	}
	var matches []ruleMatch

	for ri, r := range clusterFlavorRules {
		testMatch := false
		for _, kw := range r.testKeywords {
			if strings.Contains(lower, kw) {
				testMatch = true
				break
			}
		}
		if !testMatch {
			continue
		}
		found := false
		for i := range clusters {
			clusterLower := strings.ToLower(clusters[i].Name)
			for _, ckw := range r.clusterKeywords {
				if strings.Contains(clusterLower, ckw) {
					matches = append(matches, ruleMatch{ruleIdx: ri, cluster: &clusters[i]})
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			matches = append(matches, ruleMatch{ruleIdx: ri, cluster: nil})
		}
	}

	if len(matches) == 0 {
		return nil
	}
	firstMatch := matches[0]
	if firstMatch.cluster != nil {
		return firstMatch.cluster
	}
	return nil
}

// clusterFlavorRules maps test name keywords to cluster directory fragments.
// CAPZ-flavored rules are the engine default fallback.
var clusterFlavorRules = []struct {
	testKeywords    []string
	clusterKeywords []string
}{
	{testKeywords: []string{"flatcar"}, clusterKeywords: []string{"flatcar-sysext"}},
	{testKeywords: []string{"azure linux 3", "azurelinux 3", "azl3"}, clusterKeywords: []string{"azl3"}},
	{testKeywords: []string{"azure cni v1", "cni v1", "cni-v1"}, clusterKeywords: []string{"azure-cni-v1"}},
	{testKeywords: []string{"rke2"}, clusterKeywords: []string{"rke2"}},
	{testKeywords: []string{"clusterclass", "cluster class"}, clusterKeywords: []string{"clusterclass", "topology"}},
	{testKeywords: []string{"edgezone", "edge zone"}, clusterKeywords: []string{"edgezone"}},
	{testKeywords: []string{"nvidia", "gpu"}, clusterKeywords: []string{"nvidia-gpu"}},
	{testKeywords: []string{"spot"}, clusterKeywords: []string{"spot"}},
	{testKeywords: []string{"private"}, clusterKeywords: []string{"private"}},
	{testKeywords: []string{"custom vnet", "custom-vnet"}, clusterKeywords: []string{"custom-vnet"}},
	{testKeywords: []string{"apiserver-ilb", "internal load balancer"}, clusterKeywords: []string{"apiserver-ilb"}},
	{testKeywords: []string{"aks"}, clusterKeywords: []string{"-aks"}},
	{testKeywords: []string{"machine pool", "machinepool", "vmss"}, clusterKeywords: []string{"machine-pool"}},
	{testKeywords: []string{"dual-stack", "dualstack", "dual stack"}, clusterKeywords: []string{"dual-stack"}},
	{testKeywords: []string{"ipv6"}, clusterKeywords: []string{"ipv6"}},
	{testKeywords: []string{"windows"}, clusterKeywords: []string{"windows"}},
	{testKeywords: []string{"ha ", "ha cluster", "highly available", "highly-available"}, clusterKeywords: []string{"-ha"}},
	{testKeywords: []string{"dalec"}, clusterKeywords: []string{"dalec"}},
	{testKeywords: []string{"ci version", "ci-version", "ci artifacts"}, clusterKeywords: []string{"ci-version"}},
}

var randomSuffixRe = regexp.MustCompile(`-[a-z0-9]{4,10}$`)

func normalizeForMatch(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func clusterFlavorKey(clusterDir string) string {
	norm := normalizeForMatch(clusterDir)
	if m := randomSuffixRe.FindString(norm); m != "" {
		norm = strings.TrimSuffix(norm, m)
	}
	if len(norm) < 3 {
		return ""
	}
	return norm
}

func matchByFlavorSubstring(testName string, clusters []Cluster) *Cluster {
	normTest := normalizeForMatch(testName)
	if normTest == "" {
		return nil
	}
	bestIdx := -1
	var bestKey string
	for i := range clusters {
		key := clusterFlavorKey(clusters[i].Name)
		if key == "" || !strings.Contains(normTest, key) {
			continue
		}
		switch {
		case bestIdx == -1:
		case len(key) > len(bestKey):
		case len(key) == len(bestKey) && clusters[i].Name < clusters[bestIdx].Name:
		default:
			continue
		}
		bestIdx = i
		bestKey = key
	}
	if bestIdx == -1 {
		return nil
	}
	return &clusters[bestIdx]
}
