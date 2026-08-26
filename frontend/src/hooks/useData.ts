import { useState, useEffect } from "react";
import type {
  Dashboard,
  JobDetail,
  ResolvedState,
  SearchIndex,
} from "../types/dashboard";
import type {
  PullRequestDetail,
  PullRequestIndex,
  SharedFailureIndex,
} from "../types/pullRequests";
import { jobDataFilename } from "../lib/utils";
import { searchIndexPath } from "../lib/search";
import { normalizeFlakinessReport, type FlakinessReportWire } from "../lib/flakinessReport";

const DATA_BASE =
  import.meta.env.VITE_DATA_URL ?? `${import.meta.env.BASE_URL}data`;

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function useJSON<T>(path: string | null) {
  const [result, setResult] = useState<{
    path: string | null;
    data: T | null;
    error: string | null;
  }>({ path: null, data: null, error: null });

  useEffect(() => {
    let cancelled = false;
    if (path === null) return;
    const controller = new AbortController();

    fetch(`${DATA_BASE}/${path}`, { signal: controller.signal })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json() as Promise<T>;
      })
      .then((value) => {
        if (!cancelled) setResult({ path, data: value, error: null });
      })
      .catch((error: unknown) => {
        if (error instanceof Error && error.name === "AbortError") return;
        if (!cancelled) {
          setResult({ path, data: null, error: errorMessage(error) });
        }
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [path]);

  if (path === null) {
    return { data: null, loading: false, error: null };
  }
  if (result.path !== path) {
    return { data: null, loading: true, error: null };
  }
  return { data: result.data, loading: false, error: result.error };
}

export function useDashboard() {
  return useJSON<Dashboard>("dashboard.json");
}

export function useFlakinessReport() {
  const result = useJSON<FlakinessReportWire>("flakiness.json");
  return {
    ...result,
    data: result.data ? normalizeFlakinessReport(result.data) : null,
  };
}

export function useJobDetail(jobName: string | undefined) {
  return useJSON<JobDetail>(jobName ? `jobs/${jobDataFilename(jobName)}` : null);
}

export function useSearchIndex(activated: boolean) {
  return useJSON<SearchIndex>(searchIndexPath(activated));
}

export function usePullRequestIndex(enabled: boolean) {
  return useJSON<PullRequestIndex>(enabled ? "pull-requests.json" : null);
}

export function useSharedFailures(enabled: boolean) {
  return useJSON<SharedFailureIndex>(enabled ? "pull-request-failures.json" : null);
}

export function usePullRequestDetail(number: string | undefined) {
  const safe = number && /^\d+$/.test(number) ? number : null;
  return useJSON<PullRequestDetail>(safe ? `pull-requests/${safe}.json` : null);
}

// resolvedReadAttempts bounds the retries of one resolved-state read. A read
// that follows a dismissal write must eventually land: leaving the pre-mutation
// set in place would show a stale control until the view is remounted.
const resolvedReadAttempts = 5;

function emptyResolved(): ResolvedState {
  return { resolved: {}, causes: {} };
}

export function useResolved() {
  const [data, setData] = useState<ResolvedState>(emptyResolved);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    let attempt = 0;

    function load() {
      fetch(`${DATA_BASE}/resolved.json`, { cache: "no-store" })
        .then((r) => {
          // A missing file (static mode, or nothing resolved yet) is normal:
          // treat it as an empty set rather than an error.
          if (r.status === 404) return emptyResolved();
          if (!r.ok) throw new Error(`resolved.json: ${r.status}`);
          return r.json() as Promise<ResolvedState>;
        })
        .then((d: ResolvedState) => {
          if (cancelled) return;
          // causes is omitted entirely when nothing is resolved at that scope,
          // so it is filled in here and consumers never guard the lookup.
          setData(d?.resolved ? { resolved: d.resolved, causes: d.causes ?? {} } : emptyResolved());
          setLoading(false);
        })
        .catch(() => {
          if (cancelled) return;
          // Keep what we already have rather than reporting an empty set, and
          // retry so a transient failure resolves itself.
          if (attempt >= resolvedReadAttempts - 1) {
            setLoading(false);
            return;
          }
          timer = window.setTimeout(load, Math.min(8000, 500 * 2 ** attempt++));
        });
    }

    load();
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [nonce]);

  return { data, loading, refetch: () => setNonce((n) => n + 1) };
}
