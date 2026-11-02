import { useState, useEffect, useReducer, useRef } from "react";
import type {
  Dashboard,
  JobDetail,
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
import { initialResolvedRead, readResolvedState, resolvedReadState, resolutionAvailability } from "../lib/resolvedState";

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

export function useResolved() {
  const [state, dispatch] = useReducer(resolvedReadState, initialResolvedRead);
  const [nonce, setNonce] = useState(0);
  const activeRead = useRef<AbortController | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    activeRead.current = controller;
    void readResolvedState({
      url: `${DATA_BASE}/resolved.json`, signal: controller.signal, onResult: dispatch,
    });
    return () => controller.abort();
  }, [nonce]);

  return {
    ...state,
    availability: resolutionAvailability(state),
    refetch: () => {
      activeRead.current?.abort();
      dispatch({ type: "loading" });
      setNonce((n) => n + 1);
    },
  };
}
