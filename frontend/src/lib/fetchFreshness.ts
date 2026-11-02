import type { JobFetchStatus, JobFetchSummary } from "../types/dashboard";
import { formatFetchTimestamp } from "./fetchStatus.js";

export function fetchFreshnessDetail(fetch?: JobFetchSummary): string | null {
  if (!fetch || fetch.failed === 0) return null;
  const retained = fetch.failed - fetch.missing;
  const details = [`${fetch.failed} of ${fetch.total} jobs could not be refreshed.`];
  if (retained > 0) details.push(`${retained} ${retained === 1 ? "job shows" : "jobs show"} prior data.`);
  if (fetch.missing > 0) details.push(`${fetch.missing} ${fetch.missing === 1 ? "job has" : "jobs have"} no published data.`);
  return details.join(" ");
}

export function jobFetchFreshnessDetail(fetch?: JobFetchStatus): string | null {
  if (fetch?.state !== "carried_forward") return null;
  const lastSuccess = fetch.last_success_at
    ? `Last successful fetch ${formatFetchTimestamp(fetch.last_success_at)}`
    : "Last successful fetch unknown";
  return `${lastSuccess}. Checked ${formatFetchTimestamp(fetch.checked_at)}. Observed status and runs are from prior data.`;
}
