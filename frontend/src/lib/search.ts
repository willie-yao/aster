import Fuse from "fuse.js";
import type { SearchEntry } from "../types/dashboard";

export function searchIndexPath(activated: boolean): string | null {
  return activated ? "search-index.json" : null;
}

export function createSearchFuse(entries: SearchEntry[]) {
  return new Fuse(entries, {
    keys: ["test_name", "job_name", "tab_name"],
    threshold: 0.4,
    includeScore: true,
    ignoreLocation: true,
  });
}
