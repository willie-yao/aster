import type { Manifest } from "../types/manifest";

// DEFAULT_PERSISTENT_AFTER mirrors the engine default for
// attention.persistent_after and is used when a manifest predates the field.
export const DEFAULT_PERSISTENT_AFTER = 3;

// persistentAfter resolves the consecutive-failure count this project treats as
// a persistent failure. Anything the UI classifies client-side must use it, or
// it will disagree with the classification the backend already published.
export function persistentAfter(manifest: Manifest): number {
  const configured = manifest.attention?.persistent_after;
  return typeof configured === "number" && Number.isFinite(configured) && configured > 0
    ? configured
    : DEFAULT_PERSISTENT_AFTER;
}
