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
  failures?: TestCase[];
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
