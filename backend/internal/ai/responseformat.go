package ai

// ResponseFormatFooter is appended to every system prompt and pins the JSON
// schema the Go code unmarshals. Consumer prompts must not add their own
// schema; if the model returns anything other than this shape, the fetcher
// falls back to a Medium-severity placeholder.
//
// The wording stays tool-neutral here. Agentic-specific investigation strategy,
// including drill-down and anti-punt enforcement, lives in agToolDocs and is
// appended only when tools are wired.
const ResponseFormatFooter = `## Response Format

Always respond with a single JSON object matching this schema:

{
  "summary":        "1-2 sentence headline derived from root_cause",
  "is_transient":   true | false,
  "root_cause":     "Full causal chain from observed symptom back to the underlying cause as far as the available evidence allows. At least 3-5 sentences. Quote the exact log line(s) that prove each link in the chain and cite the artifact path each quote came from. Do NOT stop at the first error message you see; trace the chain back to the underlying cause through every layer the evidence supports. If two distinct artifacts independently support the same conclusion, cite both.",
  "severity":       "Critical" | "High" | "Medium" | "Low",
  "suggested_fix":      "Provide a concrete remediation. Name only verified source files. Do not list diagnostic or information-gathering tasks as the fix. Include exact CLI flags only when they appear in evidence you read or in an applicable project recipe. Otherwise describe the required outcome without inventing command syntax. Include one verification step. If the available evidence is insufficient, start with 'No remediation possible from available evidence:' and name the missing evidence.",
  "relevant_files":     ["source/path/read_at_the_pinned_revision.go"],
  "search_suggestions": ["unverified/path-or-name-hint"],
  "evidence_citations": [
    {"path":"artifact/path.log","line_start":2494,"line_end":2494,"quote":"exact text returned for that line"}
  ]
}

Set is_transient=true when the root cause is a transient infrastructure
issue (throttling, quota exhaustion, intermittent DNS, image-pull backoff,
disk pressure, etcd leader election, API server or node still coming up)
rather than a real bug in the code under test. If the failure matches a
transient class the project-specific knowledge calls out, set
is_transient=true even if you could keep digging for a deeper chain;
infrastructure flake is not a code bug. Reserve is_transient=false for
failures that are a genuine defect or that match no known transient class.`
