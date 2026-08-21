import type { BuildResult } from "../types/dashboard";

export interface EmptyTestResultsPresentation {
  kind: "pending" | "failed" | "unavailable" | "unreadable" | "empty" | "retained";
  title: string;
  detail: string;
  severity: "info" | "warning" | "error";
}

// retained marks a run kept only for the history strip. Its test cases were
// dropped when it aged out of the analysis window, so the absence says nothing
// about what the build reported at the time.
export function emptyTestResultsPresentation(
  run: BuildResult,
  retained = false,
): EmptyTestResultsPresentation | null {
  if ((run.test_cases?.length ?? 0) > 0) return null;

  if (retained) {
    return {
      kind: "retained",
      title: "Test results no longer stored",
      detail: "This run is older than the analysis window, so only its outcome and timing are kept. Open it in Prow for the full results.",
      severity: "info",
    };
  }

  if (run.result === "PENDING") {
    return {
      kind: "pending",
      title: "Build still running",
      detail: "Test results will appear when the build completes.",
      severity: "info",
    };
  }

  const hasJUnit = (run.junit_urls?.length ?? 0) > 0;
  if (run.junit_truncated) {
    return {
      kind: "unavailable",
      title: "Test results incomplete",
      detail: "JUnit discovery reached the artifact limit, so some test cases may be missing. Review the build artifacts and build log for the complete result.",
      severity: "warning",
    };
  }

  if (!hasJUnit && run.junit_complete === false) {
    return {
      kind: "unavailable",
      title: "Test results unavailable",
      detail: "The dashboard could not finish discovering JUnit artifacts for this run.",
      severity: "warning",
    };
  }

  if (!run.passed && !hasJUnit && run.junit_complete === true) {
    return {
      kind: "failed",
      title: "No test results were reported",
      detail: "This run failed without uploading JUnit test results. It may have stopped during setup or before the test suite could report results. Review the build log for the failure.",
      severity: "error",
    };
  }

  if (hasJUnit) {
    return {
      kind: "unreadable",
      title: "No readable test cases",
      detail: "JUnit files were uploaded, but they did not contain readable test cases.",
      severity: "warning",
    };
  }

  return {
    kind: "empty",
    title: "No JUnit test results",
    detail: "This run completed without uploading JUnit test results.",
    severity: "info",
  };
}
