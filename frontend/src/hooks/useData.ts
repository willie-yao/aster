import { useState, useEffect } from "react";
import type { AnalysisCorrectionState } from "../types/corrections";
import type {
  Dashboard,
  JobDetail,
  ResolvedState,
  SearchIndex,
} from "../types/dashboard";
import type { PullRequestDetail, PullRequestIndex } from "../types/pullRequests";
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

export function usePullRequestDetail(number: string | undefined) {
  const safe = number && /^\d+$/.test(number) ? number : null;
  return useJSON<PullRequestDetail>(safe ? `pull-requests/${safe}.json` : null);
}

export function useResolved() {
  const [data, setData] = useState<ResolvedState>({ resolved: {} });
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    fetch(`${DATA_BASE}/resolved.json`, { cache: "no-store" })
      // A missing file (static mode, or nothing resolved yet) is normal: treat
      // it as an empty set rather than an error.
      .then((r) => (r.ok ? r.json() : { resolved: {} }))
      .then((d: ResolvedState) => {
        if (!cancelled) setData(d?.resolved ? d : { resolved: {} });
      })
      .catch(() => {
        if (!cancelled) setData({ resolved: {} });
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [nonce]);

  return { data, loading, refetch: () => setNonce((n) => n + 1) };
}


export function useAnalysisCorrections() {
  const [nonce, setNonce] = useState(0);
  const [data, setData] = useState<AnalysisCorrectionState>({ corrections: {} });
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    fetch(`${DATA_BASE}/analysis_corrections.json`, { cache: "no-store" })
      .then(async (response) => {
        if (response.status === 404) return { corrections: {} };
        if (!response.ok) throw new Error(`Correction overlay returned HTTP ${response.status}`);
        return response.json() as Promise<AnalysisCorrectionState>;
      })
      .then((value) => {
        if (!cancelled) {
          setData(value?.corrections ? value : { corrections: {} });
          setError(null);
        }
      })
      .catch((loadError) => {
        if (!cancelled) setError(loadError instanceof Error ? loadError.message : "Could not load analysis corrections.");
      });
    return () => { cancelled = true; };
  }, [nonce]);
  return { data, error, refetch: () => setNonce((value) => value + 1) };
}
