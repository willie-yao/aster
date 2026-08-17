import type { TestCase } from "./dashboard";

// Observed presubmit state for one pull request, mirroring
// models.PullRequestCIState.
export type PullRequestCIState = "UNKNOWN" | "PENDING" | "PASSING" | "FAILING";

export interface PullRequestSummary {
  number: number;
  title: string;
  author: string;
  repo: string;
  base_ref: string;
  head_sha: string;
  html_url: string;
  created_at: string;
  updated_at: string;
  ci_state: PullRequestCIState;
  checks_observed: number;
  checks_failing: number;
  failing_tests: number;
}

// AttributionVerdict explains whether a failure is specific to a pull request,
// mirroring models.AttributionVerdict. No verdict asserts causation.
export type AttributionVerdict =
  | "pre_existing"
  | "widespread"
  | "known_flake"
  | "touches_changed_code"
  | "unexplained"
  | "inconclusive";

export type AttributionConfidence = "high" | "medium" | "low";

export interface AttributionEvidence {
  kind: string;
  detail: string;
  job_id?: string;
  test_name?: string;
  // Repository-relative files the claim refers to.
  paths?: string[];
}

export interface FailureAttribution {
  verdict: AttributionVerdict;
  confidence: AttributionConfidence;
  summary: string;
  evidence?: AttributionEvidence[];
}

// PullRequestFailure is one failing case with its deterministic attribution.
// TestCase fields are inlined by the backend, so they stay at the top level.
export interface PullRequestFailure extends TestCase {
  attribution?: FailureAttribution;
}

export interface PullRequestCheck {
  job_name: string;
  job_id: string;
  optional?: boolean;
  build_id: string;
  passed: boolean;
  result?: string;
  started: string;
  finished?: string;
  // The pull request head this build checked out. stale marks a build that
  // tested an older head than the pull request's current one.
  tested_sha?: string;
  stale?: boolean;
  web_url?: string;
  build_log_url?: string;
  tests_failed?: number;
  failures?: PullRequestFailure[];
  // Set when the per-check storage cap dropped some failing cases.
  failures_truncated?: boolean;
}

export interface PullRequestIndex {
  generated_at: string;
  repo: string;
  pull_requests: PullRequestSummary[];
}

export interface PullRequestDetail extends PullRequestSummary {
  generated_at: string;
  checks: PullRequestCheck[];
}
