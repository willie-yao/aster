import { useEffect } from "react";
import { matchPath } from "react-router";

const pageRoutes = [
  { path: "/", title: "Overview" },
  { path: "/flaky", title: "Failure Trends" },
  { path: "/pull-requests", title: "Pull Requests" },
  { path: "/pull-requests/:number", title: "Pull request checks" },
  { path: "/analysis-health", title: "Analysis Health" },
  { path: "/ai-usage", title: "AI Usage" },
  { path: "/job/:jobName", title: "Job details" },
  { path: "/job/:jobName/test/:testName", title: "Test details" },
  { path: "/job/:jobName/build/:buildId/failure", title: "Build failure" },
  { path: "/action-request/:requestID", title: "Draft review" },
] as const;

export function pageTitleForPath(pathname: string): string {
  return (
    pageRoutes.find(({ path }) =>
      matchPath({ path, end: true }, pathname),
    )?.title ?? "Page not found"
  );
}

export function documentTitleForPath(pathname: string, brand: string): string {
  return `${pageTitleForPath(pathname)} | ${brand}`;
}

export function usePageDocumentTitle(pathname: string, brand: string) {
  useEffect(() => {
    document.title = documentTitleForPath(pathname, brand);
  }, [brand, pathname]);
}
