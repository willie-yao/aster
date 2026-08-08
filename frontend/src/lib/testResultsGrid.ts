export type GridCellStatus = "passed" | "failed" | "skipped" | "absent";

const statusLabels: Record<GridCellStatus, string> = {
  passed: "passed",
  failed: "failed",
  skipped: "skipped",
  absent: "not reported",
};

export function gridCellAccessibleName(
  testName: string,
  buildID: string,
  started: string,
  status: GridCellStatus,
): string {
  const date = new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
  }).format(new Date(started));
  return `${testName}, run ${buildID} on ${date}, ${statusLabels[status]}`;
}

export function gridStatusSymbol(status: GridCellStatus): string {
  switch (status) {
    case "passed":
      return "✓";
    case "failed":
      return "×";
    case "skipped":
      return "–";
    default:
      return "·";
  }
}
