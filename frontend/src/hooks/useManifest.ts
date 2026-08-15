import { createContext, useContext } from "react";
import type { Manifest } from "../types/manifest";

export const ManifestContext = createContext<Manifest | null>(null);

export function useManifest(): Manifest {
  const m = useContext(ManifestContext);
  if (!m) {
    throw new Error("useManifest must be called inside <ManifestProvider>");
  }
  return m;
}
