import Box from "@mui/material/Box";
import { createElement, type ElementType, type ReactElement } from "react";
import type { JobSummary, PatternAnalysis } from "../types/dashboard";
import type { CategoryRule } from "../types/manifest";

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  const h = Math.floor(seconds / 3600);
  const m = Math.round((seconds % 3600) / 60);
  return `${h}h ${m}m`;
}

export function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const hours = Math.floor(diff / 3600000);
  if (hours < 1) return "just now";
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "1 day ago";
  return `${days} days ago`;
}

export function shortDate(dateStr: string): string {
  const d = new Date(dateStr);
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

export function formatPercent(rate: number): string {
  return `${Math.round(rate * 100)}%`;
}

type ConfidenceColor = "success" | "warning";

export function confidenceColor(
  confidence: PatternAnalysis["confidence"],
  highColor: ConfidenceColor = "warning",
): ConfidenceColor | undefined {
  if (confidence === "high") return highColor;
  if (confidence === "medium") return "warning";
  return undefined;
}

export function jobDataFilename(jobID: string): string {
  const bytes = new TextEncoder().encode(jobID);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return `${btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")}.json`;
}

export function groupByCategory(
  jobs: JobSummary[]
): Record<string, JobSummary[]> {
  const groups: Record<string, JobSummary[]> = {};
  for (const job of jobs) {
    const cat = job.category || "other";
    (groups[cat] ??= []).push(job);
  }
  return groups;
}

export function groupByBranch(
  jobs: JobSummary[]
): Record<string, JobSummary[]> {
  const groups: Record<string, JobSummary[]> = {};
  for (const job of jobs) {
    const branch = job.branch || "unknown";
    (groups[branch] ??= []).push(job);
  }
  return groups;
}

/**
 * Build a category-id to label map, deduplicating by id. Returns just the
 * implicit "Other" entry when the manifest declares no rules.
 */
export function categoryLabelsFromRules(
  rules: CategoryRule[] | undefined
): Record<string, string> {
  const out: Record<string, string> = { other: "Other" };
  for (const r of rules ?? []) {
    if (r.id && !(r.id in out)) out[r.id] = r.label || r.id;
  }
  return out;
}

/** Build an ordered list of category ids matching the manifest's rule order. */
export function categoryOrderFromRules(
  rules: CategoryRule[] | undefined
): string[] {
  const seen = new Set<string>();
  const order: string[] = [];
  for (const r of rules ?? []) {
    if (r.id && !seen.has(r.id)) {
      seen.add(r.id);
      order.push(r.id);
    }
  }
  order.push("other");
  return order;
}

/**
 * Build an ordered list of category ids for display. When the manifest
 * declares `category_display_order`, those ids come first in declared order,
 * followed by any remaining ids in rule-declaration order, with "other"
 * implicitly last.
 */
export function categoryDisplayOrder(
  rules: CategoryRule[] | undefined,
  explicit: string[] | undefined
): string[] {
  const ruleOrder = categoryOrderFromRules(rules);
  if (!explicit || explicit.length === 0) return ruleOrder;
  const seen = new Set<string>();
  const out: string[] = [];
  for (const id of explicit) {
    if (id && !seen.has(id)) {
      seen.add(id);
      out.push(id);
    }
  }
  for (const id of ruleOrder) {
    if (!seen.has(id)) {
      seen.add(id);
      out.push(id);
    }
  }
  return out;
}

/** Split text with inline numbered/bulleted steps into separate lines. */
export function formatSteps(text: string): string {
  // Insert newlines before numbered steps after the first item.
  let result = text.replace(/\s+(\d+)\.\s/g, (match, num) => {
    return Number(num) > 1 ? `\n${num}. ` : match;
  });
  // Insert newlines before parenthesized step numbers when preceded by prose.
  result = result.replace(/([.!?:])?\s+\((\d+)\)\s/g, (_match, punct, num) => {
    return `${punct || ""}\n(${num}) `;
  });
  // Insert newlines before bullet markers.
  result = result.replace(/\s+[-•]\s/g, "\n• ");
  return result;
}

/** Strip common verbose prefixes from test names for compact display. */
export function shortTestName(name: string): string {
  const short = name
    .replace(/^\[It\]\s+/, "")
    .replace(/^Workload cluster creation\s+Creating\s+(a\s+)?/, "")
    .replace(/^Running the Cluster API E2E tests\s+/, "CAPI: ")
    .replace(/^Conformance Tests\s+/, "Conformance: ")
    .replace(/^Running\s+/, "");
  return short.charAt(0).toUpperCase() + short.slice(1);
}

/** Strip the derived short-name prefix off a job name for compact display. */
export function shortJobName(name: string, shortNamePrefix: string): string {
  if (!shortNamePrefix) return name;
  return name.startsWith(shortNamePrefix) ? name.slice(shortNamePrefix.length) : name;
}

const goFileLineRe = /([a-zA-Z0-9_/.\-@]+\.go:\d+)/g;
const stackTraceHighlightSx = { color: "primary.main" } as const;
const StackTraceSpan = Box as ElementType;

export function highlightStackTrace(body: string): (string | ReactElement)[] {
  const parts: (string | ReactElement)[] = [];
  let lastIndex = 0;
  let match: RegExpExecArray | null;
  let key = 0;

  while ((match = goFileLineRe.exec(body)) !== null) {
    if (match.index > lastIndex) {
      parts.push(body.slice(lastIndex, match.index));
    }
    parts.push(
      createElement(
        StackTraceSpan,
        { component: "span", key: key++, sx: stackTraceHighlightSx },
        match[1],
      ),
    );
    lastIndex = match.index + match[0].length;
  }
  if (lastIndex < body.length) {
    parts.push(body.slice(lastIndex));
  }
  return parts;
}

export interface FileToUrlContext {
  buildLogUrl?: string;
  clusterArtifacts?: { machines?: { logs: Record<string, string> }[] };
  /**
   * Web URL root for the current build, such as
   * `https://gcsweb.k8s.io/gcs/<bucket>/logs/<job>/<id>/`. Unmatched artifact
   * paths resolve under this root, with `artifacts/` prepended when needed.
   */
  webUrl?: string;
  /**
   * Verified GitHub links for source-file citations keyed by cleaned path.
   * Source citations link only when present here, so absent files stay plain.
   */
  fileLinks?: Record<string, string>;
}

/** Turn a file path from AI analysis into a URL if possible. */
export function fileToUrl(
  filePath: string,
  context?: FileToUrlContext
): string | null {
  const clean = filePath.replace(/\s*\(.*\)$/, "").trim();
  const lower = clean.toLowerCase();

  // Match artifact/log references to actual URLs from context.
  if (context) {
    if (/build-log/i.test(lower) && context.buildLogUrl) {
      return context.buildLogUrl;
    }
    const machines = context.clusterArtifacts?.machines;
    if (machines && machines.length > 0) {
      const logs = machines[0].logs;
      if (/cloud-init-output/i.test(lower) && logs["cloud-init-output.log"]) {
        return logs["cloud-init-output.log"];
      }
      if (/cloud-init\.log/i.test(lower) && logs["cloud-init.log"]) {
        return logs["cloud-init.log"];
      }
      if (/boot\.log/i.test(lower) && logs["boot.log"]) {
        return logs["boot.log"];
      }
      if (/kubelet\.log/i.test(lower) && logs["kubelet.log"]) {
        return logs["kubelet.log"];
      }
      if (/kube-apiserver/i.test(lower) && logs["kube-apiserver.log"]) {
        return logs["kube-apiserver.log"];
      }
      if (/journal\.log/i.test(lower) && logs["journal.log"]) {
        return logs["journal.log"];
      }
      if (/containerd/i.test(lower) && logs["containerd.log"]) {
        return logs["containerd.log"];
      }
    }
  }

  // Source-file links use the backend-verified map. This keeps UI path handling
  // generic and avoids links to files that do not exist.
  if (context?.fileLinks && context.fileLinks[clean]) {
    return context.fileLinks[clean];
  }

  // Resolve plausible GCS artifact paths against the build web URL. Only
  // artifact roots such as artifacts/... or clusters/... are eligible, so
  // host-like and repo-doc tokens are not turned into bogus GCS URLs.
  if (context?.webUrl) {
    const path = clean.replace(/^\/+/, "");
    const isArtifactRoot = /^(artifacts|clusters)\//.test(path);
    const knownExt = /\.(log|yaml|yml|json|xml|txt|out|conf)$/i.test(path);
    if (isArtifactRoot && knownExt) {
      const full = path.startsWith("artifacts/") ? path : `artifacts/${path}`;
      const base = context.webUrl.replace(/\/+$/, "");
      return `${base}/${full}`;
    }
  }

  return null;
}

/** Return sort priority: 0 GCS artifact, 1 source repo, 2 other or unlinked. */
export function fileSortKey(
  filePath: string,
  context?: FileToUrlContext
): number {
  const url = fileToUrl(filePath, context);
  if (!url) return 2;
  if (url.includes("storage.googleapis.com") || url.includes("gcsweb.k8s.io")) return 0;
  if (url.includes("github.com")) return 1;
  return 2;
}
