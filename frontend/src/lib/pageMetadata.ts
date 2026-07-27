import { useEffect } from "react";
import { matchPath } from "react-router-dom";

const pageRoutes = [
  { path: "/", title: "Overview" },
  { path: "/flaky", title: "Test Analysis" },
  { path: "/analysis-traces", title: "Analysis Traces" },
  { path: "/job/:jobName", title: "Job Details" },
  { path: "/job/:jobName/test/:testName", title: "Test Details" },
  { path: "/action-request/:requestID", title: "Action Request" },
] as const;

export function pageTitleForPath(pathname: string): string {
  return (
    pageRoutes.find(({ path }) =>
      matchPath({ path, end: true }, pathname),
    )?.title ?? "Page Not Found"
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
