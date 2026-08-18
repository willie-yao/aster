import type { SharedFailure, SharedFailureMember } from "../types/pullRequests.js";

// orderSharedFailures leads with the failure hitting the most pull requests,
// then the most recently observed. The remaining comparisons are on the
// correlation key, so a pass that observes nothing new keeps the same order.
export function orderSharedFailures(failures: SharedFailure[]): SharedFailure[] {
  return [...failures].sort((a, b) => {
    const byWidth = b.pull_requests.length - a.pull_requests.length;
    if (byWidth !== 0) return byWidth;
    const byRecency =
      Date.parse(b.newest_build_started) - Date.parse(a.newest_build_started);
    if (!Number.isNaN(byRecency) && byRecency !== 0) return byRecency;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

// sharedFailureSubject names what failed. A build-level failure carries a
// generic test name, so the job is the useful subject.
export function sharedFailureSubject(failure: SharedFailure): string {
  return failure.build_level ? failure.job_name : failure.test_name;
}

// sharedFailureScope describes how many pull requests the failure spans, which
// is the number that makes it worth looking at.
export function sharedFailureScope(failure: SharedFailure): string {
  const count = failure.pull_requests.length;
  const pulls = count === 1 ? "1 pull request" : `${count} pull requests`;
  return failure.base_ref ? `${pulls} targeting ${failure.base_ref}` : pulls;
}

// findSharedFailure locates one cluster by its published id.
export function findSharedFailure(
  failures: SharedFailure[] | undefined,
  id: string | undefined,
): SharedFailure | undefined {
  if (!failures || !id) return undefined;
  return failures.find((failure) => failure.id === id);
}

// findSharedFailureFor locates the cluster a pull request failure belongs to.
// The id is a hash the frontend cannot recompute, so the lookup is on the
// correlation key the backend published alongside it.
export function findSharedFailureFor(
  failures: SharedFailure[] | undefined,
  baseRef: string,
  jobName: string,
  testName: string,
): SharedFailure | undefined {
  if (!failures) return undefined;
  return failures.find(
    (failure) =>
      failure.base_ref === baseRef &&
      failure.job_name === jobName &&
      failure.test_name === testName,
  );
}

// evidenceMember is the build a cluster escalation would read: the most recent
// finished build that tested its pull request's current head. It mirrors the
// server's choice so the page names the same subject the analysis will.
export function evidenceMember(failure: SharedFailure): SharedFailureMember | undefined {
  let best: SharedFailureMember | undefined;
  for (const member of failure.pull_requests) {
    if (member.stale || !member.finished || !member.build_id) continue;
    if (!best || Date.parse(member.started) > Date.parse(best.started)) {
      best = member;
    }
  }
  return best;
}

// sharedFailureAnalyzable reports whether one escalation could run right now:
// the cluster must be the only remaining path, and some build must be readable.
export function sharedFailureAnalyzable(failure: SharedFailure): boolean {
  return failure.escalatable && evidenceMember(failure) !== undefined;
}

// sharedFailureBlockedReason explains why no analysis is offered, so the page
// says what is missing rather than hiding the control without comment.
export function sharedFailureBlockedReason(failure: SharedFailure): string | null {
  if (!failure.escalatable) {
    return "This failure can be investigated from one of the affected pull requests, so it is analyzed there rather than here.";
  }
  if (!evidenceMember(failure)) {
    return "No affected pull request has a finished build on its current head yet, so there are no artifacts to read.";
  }
  return null;
}
