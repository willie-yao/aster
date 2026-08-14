package onboard

import (
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/project"
)

// maxCategories caps inferred rules so noisy fleets stay reviewable.
const maxCategories = 8

// tokenSplit breaks a job name into lowercase tokens on any non-alphanumeric
// boundary.
var tokenSplit = regexp.MustCompile(`[^a-z0-9]+`)

// noiseToken matches tokens that carry no grouping signal: pure numbers,
// version markers, Prow job-kind prefixes, and the reserved "other" id.
// Tokens like "e2e" and "ci" can be meaningful, so frequency checks handle them.
var noiseToken = regexp.MustCompile(`^(\d+|v\d.*|release|main|master|periodic|presubmit|other)$`)

// InferCategories proposes an ordered set of {match, id, label} rules by
// clustering the job names on their distinguishing tokens. It is deterministic
// and meant as a reviewable starting point, not a perfect taxonomy: a token
// that partitions the fleet becomes a category. Narrow groups come first to
// match the engine's first-match-wins semantics. Returns nil for a flat grid.
func InferCategories(jobNames []string) []project.CategoryRule {
	n := len(jobNames)
	if n < 2 {
		return nil
	}

	// Map each token to the job indices containing it.
	tokenJobs := map[string]map[int]bool{}
	for i, name := range jobNames {
		seen := map[string]bool{}
		for _, tok := range tokenSplit.Split(strings.ToLower(name), -1) {
			if tok == "" || seen[tok] || noiseToken.MatchString(tok) {
				continue
			}
			seen[tok] = true
			if tokenJobs[tok] == nil {
				tokenJobs[tok] = map[int]bool{}
			}
			tokenJobs[tok][i] = true
		}
	}

	// Candidate tokens appear in at least 2 jobs but not every job.
	var candTokens []string
	for tok, jobs := range tokenJobs {
		if len(jobs) >= 2 && len(jobs) < n {
			candTokens = append(candTokens, tok)
		}
	}

	// Use runtime substring semantics so proposed rules classify jobs the same
	// way project.Categorize will.
	lowerNames := make([]string, n)
	for i, name := range jobNames {
		lowerNames[i] = strings.ToLower(name)
	}
	substringJobs := func(tok string) map[int]bool {
		set := map[int]bool{}
		for i, ln := range lowerNames {
			if strings.Contains(ln, tok) {
				set[i] = true
			}
		}
		return set
	}

	type cand struct {
		token string
		jobs  map[int]bool
	}
	cands := make([]cand, 0, len(candTokens))
	for _, tok := range candTokens {
		cands = append(cands, cand{token: tok, jobs: substringJobs(tok)})
	}
	// Most specific first, then alphabetical for stability.
	sort.Slice(cands, func(i, j int) bool {
		if len(cands[i].jobs) != len(cands[j].jobs) {
			return len(cands[i].jobs) < len(cands[j].jobs)
		}
		return cands[i].token < cands[j].token
	})

	// Greedily keep a candidate only if it covers a job no earlier rule did, so
	// the set stays compact and the rules don't all target the same jobs.
	covered := map[int]bool{}
	var rules []project.CategoryRule
	for _, c := range cands {
		if len(rules) >= maxCategories {
			break
		}
		addsNew := false
		for idx := range c.jobs {
			if !covered[idx] {
				addsNew = true
				break
			}
		}
		if !addsNew {
			continue
		}
		for idx := range c.jobs {
			covered[idx] = true
		}
		rules = append(rules, project.CategoryRule{
			Match: c.token,
			ID:    c.token,
			Label: labelFor(c.token),
		})
	}
	return rules
}

// labelFor renders a token as a title-cased label with known acronyms.
func labelFor(token string) string {
	parts := strings.FieldsFunc(token, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if up, ok := acronyms[p]; ok {
			parts[i] = up
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// acronyms are tokens rendered upper-case in labels.
var acronyms = map[string]string{
	"aks": "AKS", "gpu": "GPU", "csi": "CSI", "cni": "CNI", "api": "API",
	"vm": "VM", "vmss": "VMSS", "ipv6": "IPv6", "rke2": "RKE2", "capi": "CAPI",
	"capz": "CAPZ", "ip": "IP", "lb": "LB", "ha": "HA", "ci": "CI", "e2e": "E2E",
	"aso": "ASO", "byo": "BYO", "ilb": "ILB",
}
