import { useState, useEffect } from "react";
import type { Dashboard, FlakinessReport, JobDetail, ResolvedState, SearchIndex } from "../types/dashboard";

const DATA_BASE =
  import.meta.env.VITE_DATA_URL ?? `${import.meta.env.BASE_URL}data`;

export function useDashboard() {
  const [data, setData] = useState<Dashboard | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetch(`${DATA_BASE}/dashboard.json`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json();
      })
      .then(setData)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  return { data, loading, error };
}

export function useFlakinessReport() {
  const [data, setData] = useState<FlakinessReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetch(`${DATA_BASE}/flakiness.json`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json();
      })
      .then(setData)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  return { data, loading, error };
}

export function useJobDetail(jobName: string | undefined) {
  const [data, setData] = useState<JobDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!jobName) return;
    const sanitized = jobName.replace(/[^a-zA-Z0-9\-_]/g, "-");
    fetch(`${DATA_BASE}/jobs/${sanitized}.json`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json();
      })
      .then(setData)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [jobName]);

  return { data, loading, error };
}

export function useSearchIndex() {
  const [data, setData] = useState<SearchIndex | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetch(`${DATA_BASE}/search-index.json`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json();
      })
      .then(setData)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  return { data, loading, error };
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
