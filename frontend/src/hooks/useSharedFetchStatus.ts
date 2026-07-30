import { createContext, useContext } from "react";
import type { FetchStatusResponse } from "../types/fetchStatus";

export const FetchStatusContext = createContext<FetchStatusResponse | null>(null);

export function useSharedFetchStatus(): FetchStatusResponse | null {
  return useContext(FetchStatusContext);
}
