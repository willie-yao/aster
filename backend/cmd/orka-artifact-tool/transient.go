package main

import (
	"log"
	"net/http"
	"regexp"
	"strings"
)

const (
	transientTailLines              = 5000
	transientTailBytes              = 256 * 1024
	maxTransientMatchesPerSignature = 5
	maxTransientLineLen             = 300
)

type transientSignature struct {
	Class string
	Re    *regexp.Regexp
}

type transientMatch struct {
	Class   string `json:"class"`
	Pattern string `json:"pattern"`
	Line    string `json:"line"`
	Path    string `json:"path"`
}

var transientSignatures = []transientSignature{
	{Class: "throttling", Re: regexp.MustCompile(`(?i)\b429\b|too many requests|throttl`)},
	{Class: "quota", Re: regexp.MustCompile(`(?i)quota (exceeded|limit)|OperationNotAllowed`)},
	{Class: "dns", Re: regexp.MustCompile(`(?i)no such host|dial tcp: lookup|temporary failure in name resolution|lookup .* on .*: (server misbehaving|i/o timeout)`)},
	{Class: "apiserver_starting", Re: regexp.MustCompile(`(?i)the server was unable to return a response in the time allotted|(:6443|api ?server).{0,40}(connection refused|was refused)`)},
	{Class: "etcd_formation", Re: regexp.MustCompile(`(?i)etcdserver:? (no leader|request timed out)|waiting for leader`)},
	{Class: "node_registration", Re: regexp.MustCompile(`(?i)node ".*" not found|failed to get provider ?id|Failed to get nodeLease`)},
	{Class: "metadata_service", Re: regexp.MustCompile(`(?i)url_helper\.py.*retry`)},
	{Class: "webhook_cert_race", Re: regexp.MustCompile(`(?i)failed to call webhook.*x509: certificate signed by unknown authority|capz-webhook-service`)},
	{Class: "image_pull_backoff", Re: regexp.MustCompile(`(?i)ImagePullBackOff|ErrImagePull`)},
	{Class: "context_deadline_cleanup", Re: regexp.MustCompile(`(?i)context deadline exceeded`)},
}

func init() {
	registerQTool("/tool/check_transient_signatures", checkTransientSignatures)
}

func checkTransientSignatures(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Paths []string `json:"paths"`
		Text  string   `json:"text"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := requestCtx(r)
	defer cancel()

	scanned := 0
	unreadable := []string{}
	matches := []transientMatch{}
	classSet := map[string]bool{}

	for _, p := range args.Paths {
		if p == "" {
			continue
		}
		tail, err := env.browser.Tail(ctx, p, transientTailLines, transientTailBytes)
		if err != nil {
			unreadable = append(unreadable, p)
			continue
		}
		scanned++
		scanTransientSource(p, string(tail.Content), &matches, classSet)
	}
	if args.Text != "" {
		scanned++
		scanTransientSource("text", args.Text, &matches, classSet)
	}

	classes := []string{}
	for _, sig := range transientSignatures {
		if classSet[sig.Class] {
			classes = append(classes, sig.Class)
		}
	}
	log.Printf("🔎 check_transient_signatures sources=%d matched=%d classes=%v", scanned, len(matches), classes)
	writeJSON(w, map[string]any{
		"scanned":    scanned,
		"any":        len(matches) > 0,
		"classes":    classes,
		"matched":    matches,
		"unreadable": unreadable,
	})
}

func scanTransientSource(path, content string, matches *[]transientMatch, classSet map[string]bool) {
	lines := strings.Split(content, "\n")
	for _, sig := range transientSignatures {
		found := 0
		for _, line := range lines {
			if found >= maxTransientMatchesPerSignature {
				break
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || !sig.Re.MatchString(trimmed) {
				continue
			}
			*matches = append(*matches, transientMatch{
				Class:   sig.Class,
				Pattern: sig.Re.String(),
				Line:    capTransientLine(trimmed),
				Path:    path,
			})
			classSet[sig.Class] = true
			found++
		}
	}
}

func capTransientLine(line string) string {
	runes := []rune(line)
	if len(runes) <= maxTransientLineLen {
		return line
	}
	return string(runes[:maxTransientLineLen-3]) + "..."
}
