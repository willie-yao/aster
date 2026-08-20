import type { FailureRecurrence } from "../types/dashboard";

// A cause seen once is exactly what the run history already shows, so recurrence
// only says something new from the second failure onwards.
const MIN_OCCURRENCES = 2;

// usable keeps only history the current window cannot already tell you. A cause
// whose every failure is on screen adds nothing; one that has failed in builds
// the window no longer reaches is the whole point.
function usable(entry: FailureRecurrence | undefined): FailureRecurrence | null {
  if (!entry || entry.occurrences < MIN_OCCURRENCES) return null;
  return entry.occurrences > (entry.builds?.length ?? 0) ? entry : null;
}

// recurrenceForBuilds finds the durable history behind a causal group. It matches
// on the group's builds rather than its signature, because a group is identified
// by the verdict signature, which preserves numbers, while recurrence is counted
// under an identity that collapses them. Where several histories cover the
// group's builds, the longest-running one is what the maintainer needs to see.
export function recurrenceForBuilds(
  recurrence: FailureRecurrence[] | undefined,
  builds: string[] | undefined,
): FailureRecurrence | null {
  if (!recurrence || !builds?.length) return null;
  let longest: FailureRecurrence | null = null;
  for (const entry of recurrence) {
    if (!entry.builds?.some((build) => builds.includes(build))) continue;
    // Filtering before ranking, not after: a fully visible cause can outrank one
    // reaching past the window, and picking it first would hide real history.
    const candidate = usable(entry);
    if (candidate && (!longest || candidate.occurrences > longest.occurrences)) {
      longest = candidate;
    }
  }
  return longest;
}

// recurrenceForBuild finds the durable history behind one failed build. A build
// too isolated to correlate into a causal group still carries its own signature,
// so this is how an infrequent flake's real age surfaces at all.
export function recurrenceForBuild(
  recurrence: FailureRecurrence[] | undefined,
  buildID: string | undefined,
): FailureRecurrence | null {
  if (!recurrence || !buildID) return null;
  return usable(recurrence.find((entry) => entry.builds?.includes(buildID)));
}

// describeRecurrence reads as a sentence fragment: "8 similar failures since Mar
// 2026". It says "similar" on purpose: the identity behind the count groups
// failures that look alike, which is evidence of a recurring problem rather than
// proof of a single shared cause.
export function describeRecurrence(entry: FailureRecurrence): string {
  const failures = `${entry.occurrences} similar failure${entry.occurrences === 1 ? "" : "s"}`;
  const since = new Date(entry.first_seen);
  if (Number.isNaN(since.getTime())) return failures;
  return `${failures} since ${since.toLocaleDateString(undefined, {
    month: "short",
    year: "numeric",
  })}`;
}
