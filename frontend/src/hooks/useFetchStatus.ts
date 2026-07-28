import { useEffect, useState } from "react";
import { useAuth } from "./useAuth";
import { useCapabilities } from "./useCapabilities";
import { pollFetchStatus } from "../lib/fetchStatus";
import type { FetchStatusResponse } from "../types/fetchStatus";

const API_BASE = import.meta.env.BASE_URL;

export function useFetchStatus(): FetchStatusResponse | null {
  const auth = useAuth();
  const { features } = useCapabilities();
  const [status, setStatus] = useState<FetchStatusResponse | null>(null);

  const enabled = Boolean(features.fetch_status) && auth.status === "authenticated";

  useEffect(() => {
    if (!enabled) return;
    const controller = new AbortController();
    void pollFetchStatus({
      url: `${API_BASE}api/fetch-status`,
      signal: controller.signal,
      onStatus: setStatus,
    });
    return () => controller.abort();
  }, [enabled]);

  return enabled ? status : null;
}
