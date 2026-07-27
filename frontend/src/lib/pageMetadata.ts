import { useEffect } from "react";

export function pageTitleForPath(pathname: string): string {
  const segments = pathname
    .split("/")
    .filter(Boolean)
    .map((segment) => segment.toLowerCase());

  if (segments.length === 0) return "Overview";
  if (segments.length === 1 && segments[0] === "flaky") return "Test Analysis";
  if (segments.length === 1 && segments[0] === "analysis-traces") {
    return "Analysis Traces";
  }
  if (segments.length === 2 && segments[0] === "job") return "Job Details";
  if (
    segments.length === 4 &&
    segments[0] === "job" &&
    segments[2] === "test"
  ) {
    return "Test Details";
  }
  if (segments.length === 2 && segments[0] === "action-request") {
    return "Action Request";
  }
  return "Page Not Found";
}

export function documentTitleForPath(pathname: string, brand: string): string {
  return `${pageTitleForPath(pathname)} | ${brand}`;
}

export function usePageDocumentTitle(pathname: string, brand: string) {
  useEffect(() => {
    document.title = documentTitleForPath(pathname, brand);
  }, [brand, pathname]);
}
