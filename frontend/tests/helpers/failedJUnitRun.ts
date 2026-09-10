import type { BuildResult } from "../../src/types/dashboard.js";

export function failedJUnitRun(buildID: string, testName: string): BuildResult {
  return {
    build_id: buildID,
    job_name: "periodic-capz-e2e-main",
    started: "2026-08-18T00:00:00Z",
    finished: "2026-08-18T01:00:00Z",
    passed: false,
    result: "FAILURE",
    duration_seconds: 3600,
    commit: "abc123",
    prow_url: "https://prow.example",
    web_url: "https://gcsweb.example",
    build_log_url: "https://gcsweb.example/build-log.txt",
    test_cases: [{
      name: testName,
      status: "failed",
      duration_seconds: 1,
      junit_file: "artifacts/junit_01.xml",
      ai_analysis: {
        generated_at: "2026-08-18T00:00:00Z",
        root_cause: "cause",
        severity: "high",
        suggested_fix: "fix",
        disposition: "citations_verified",
        file_links: { "a/b.go": "https://github.com/o/r/blob/rev/a/b.go" },
      },
    }],
    tests_total: 1,
    tests_passed: 0,
    tests_failed: 1,
    tests_skipped: 0,
  };
}
