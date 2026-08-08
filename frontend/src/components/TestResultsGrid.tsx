import { useMemo, useState } from "react";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { BuildResult } from "../types/dashboard";
import { jobRunPath, testPath } from "../lib/routes";
import { shortDate, shortTestName } from "../lib/utils";
import { Panel } from "./Panel";
import { junitTestCases } from "../lib/buildFailures";
import {
  gridCellAccessibleName,
  gridStatusSymbol,
  type GridCellStatus,
} from "../lib/testResultsGrid";
import {
  initialProgressiveCount,
  nextProgressiveCount,
  trailingWindowStart,
} from "../lib/progressive";

interface TestResultsGridProps {
  runs: BuildResult[];
  jobID: string;
}

interface GridRow {
  testName: string;
  failCount: number;
  cells: GridCellStatus[];
}

const setupPatterns =
  /^(SynchronizedBeforeSuite|SynchronizedAfterSuite|BeforeSuite|AfterSuite)$/i;
const rowBatchSize = 50;
const runBatchSize = 12;

export function TestResultsGrid({ runs, jobID }: TestResultsGridProps) {
  const sortedRuns = useMemo(
    () =>
      [...runs].sort(
        (a, b) =>
          new Date(a.started).getTime() - new Date(b.started).getTime(),
      ),
    [runs],
  );

  const gridRows = useMemo(() => {
    if (sortedRuns.length === 0) return [];

    const testMap = new Map<string, GridCellStatus[]>();

    for (let col = 0; col < sortedRuns.length; col++) {
      const run = sortedRuns[col];
      for (const tc of junitTestCases(run.test_cases)) {
        if (!testMap.has(tc.name)) {
          testMap.set(tc.name, new Array(sortedRuns.length).fill("absent"));
        }
        testMap.get(tc.name)![col] = tc.status;
      }
    }

    const rows: GridRow[] = [];

    for (const [testName, cells] of testMap) {
      const failCount = cells.filter((status) => status === "failed").length;
      const hasPass = cells.some((status) => status === "passed");
      const hasFail = failCount > 0;

      // Filter out skipped-only tests and setup/teardown unless failed.
      if (!hasPass && !hasFail) continue;
      if (setupPatterns.test(testName) && !hasFail) continue;

      rows.push({ testName, failCount, cells });
    }

    rows.sort((a, b) => {
      if (b.failCount !== a.failCount) return b.failCount - a.failCount;
      return a.testName.localeCompare(b.testName);
    });

    return rows;
  }, [sortedRuns]);

  const windowKey = `${jobID}:${sortedRuns.map((run) => run.build_id).join(",")}`;
  const [window, setWindow] = useState({ key: "", rows: rowBatchSize, runs: runBatchSize });
  const visibleRowCount =
    window.key === windowKey
      ? Math.min(gridRows.length, window.rows)
      : initialProgressiveCount(gridRows.length, rowBatchSize);
  const visibleRunCount =
    window.key === windowKey
      ? Math.min(sortedRuns.length, window.runs)
      : initialProgressiveCount(sortedRuns.length, runBatchSize);
  const runStart = trailingWindowStart(sortedRuns.length, visibleRunCount);
  const visibleRows = gridRows.slice(0, visibleRowCount);
  const visibleRuns = sortedRuns.slice(runStart);
  const hiddenRows = gridRows.length - visibleRowCount;
  const hiddenRuns = sortedRuns.length - visibleRunCount;

  function showMoreRows() {
    setWindow({
      key: windowKey,
      rows: nextProgressiveCount(visibleRowCount, gridRows.length, rowBatchSize),
      runs: visibleRunCount,
    });
  }

  function showMoreRuns() {
    setWindow({
      key: windowKey,
      rows: visibleRowCount,
      runs: nextProgressiveCount(visibleRunCount, sortedRuns.length, runBatchSize),
    });
  }

  if (runs.length === 0 || gridRows.length === 0) {
    return (
      <Panel sx={{ border: 0, borderRadius: 0, p: 3, textAlign: "center" }}>
        <Typography variant="body2" color="text.secondary">
          {runs.length === 0
            ? "No runs available."
            : "All tests passed across all runs; nothing to display."}
        </Typography>
      </Panel>
    );
  }

  const summary = `Showing ${visibleRowCount.toLocaleString()} of ${gridRows.length.toLocaleString()} tests across ${visibleRunCount.toLocaleString()} of ${sortedRuns.length.toLocaleString()} runs.`;

  return (
    <>
      <Panel
        sx={{
          display: { xs: "block", md: "none" },
          border: 0,
          borderRadius: 0,
          p: 3,
          textAlign: "center",
        }}
      >
        <Typography variant="body2" color="text.secondary">
          View on desktop for full test results grid
        </Typography>
      </Panel>

      <Panel
        sx={{
          display: { xs: "none", md: "block" },
          border: 0,
          borderRadius: 0,
          overflow: "hidden",
          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.main,
        }}
      >
        <Box
          sx={{
            display: "flex",
            alignItems: "center",
            flexWrap: "wrap",
            gap: 1,
            px: 1.5,
            py: 1,
            borderBottom: 1,
            borderColor: "divider",
          }}
        >
          <Typography role="status" color="text.secondary" sx={{ fontSize: "0.75rem" }}>
            {summary}
          </Typography>
          <Box sx={{ ml: "auto", display: "flex", flexWrap: "wrap", gap: 1 }}>
            {hiddenRuns > 0 && (
              <Button size="small" variant="outlined" onClick={showMoreRuns}>
                Show {Math.min(runBatchSize, hiddenRuns)} older runs
              </Button>
            )}
            {hiddenRows > 0 && (
              <Button size="small" variant="outlined" onClick={showMoreRows}>
                Show {Math.min(rowBatchSize, hiddenRows)} more tests
              </Button>
            )}
          </Box>
        </Box>

        <Box sx={{ display: "flex" }}>
          <Box
            sx={{
              width: 300,
              flexShrink: 0,
              overflowX: "auto",
              borderRight: 1,
              borderColor: "divider",
            }}
          >
            <Box component="table" sx={{ width: "100%", borderCollapse: "collapse" }}>
              <Box component="thead">
                <Box component="tr" sx={{ height: 32 }}>
                  <Box
                    component="th"
                    scope="col"
                    sx={{
                      bgcolor: (theme) => (theme.vars ?? theme).palette.surface.main,
                      px: 1.5,
                      textAlign: "left",
                      typography: "label",
                      fontSize: "0.625rem",
                      fontWeight: 400,
                      color: "text.secondary",
                      whiteSpace: "nowrap",
                    }}
                  >
                    Test
                  </Box>
                </Box>
              </Box>
              <Box component="tbody">
                {visibleRows.map((row) => (
                  <Box
                    component="tr"
                    key={row.testName}
                    sx={{
                      height: 28,
                      transition: (theme) => theme.transitions.create("background-color"),
                      "&:hover td": {
                        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                      },
                    }}
                  >
                    <Box
                      component="th"
                      scope="row"
                      sx={{ bgcolor: (theme) => (theme.vars ?? theme).palette.surface.main, p: 0, fontWeight: 400 }}
                    >
                      <Link
                        component={RouterLink}
                        to={testPath(jobID, row.testName)}
                        underline="none"
                        title={row.testName}
                        sx={{
                          display: "block",
                          px: 1.5,
                          py: 0.5,
                          color: "text.primary",
                          fontSize: "0.75rem",
                          whiteSpace: "nowrap",
                          transition: (theme) => theme.transitions.create("color"),
                          "&:hover": { color: "primary.main" },
                          "&:focus-visible": {
                            outline: "2px solid",
                            outlineColor: "primary.main",
                            outlineOffset: -2,
                          },
                        }}
                      >
                        {shortTestName(row.testName)}
                      </Link>
                    </Box>
                  </Box>
                ))}
              </Box>
            </Box>
          </Box>

          <Box sx={{ minWidth: 0, flex: 1, overflowX: "auto" }}>
            <Box component="table" sx={{ borderCollapse: "collapse" }}>
              <Box component="thead">
                <Box component="tr" sx={{ height: 32 }}>
                  {visibleRuns.map((run) => (
                    <Box
                      component="th"
                      scope="col"
                      key={run.build_id}
                      title={`Run ${run.build_id}`}
                      sx={{
                        px: 0.5,
                        typography: "label",
                        fontSize: "0.625rem",
                        fontWeight: 400,
                        color: "text.secondary",
                      }}
                    >
                      {shortDate(run.started)}
                    </Box>
                  ))}
                </Box>
              </Box>
              <Box component="tbody">
                {visibleRows.map((row) => (
                  <Box
                    component="tr"
                    key={row.testName}
                    sx={{
                      height: 28,
                      transition: (theme) => theme.transitions.create("background-color"),
                      "&:hover td": {
                        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                      },
                    }}
                  >
                    {row.cells.slice(runStart).map((status, colIdx) => {
                      const run = visibleRuns[colIdx];
                      const label = gridCellAccessibleName(
                        row.testName,
                        run.build_id,
                        run.started,
                        status,
                      );
                      const cellColor =
                        status === "passed"
                          ? "success.main"
                          : status === "failed"
                            ? "error.main"
                            : status === "skipped"
                              ? "action.disabledBackground"
                              : "transparent";
                      const cellTextColor =
                        status === "passed"
                          ? "success.contrastText"
                          : status === "failed"
                            ? "error.contrastText"
                            : "text.secondary";

                      const cell = (
                        <Box
                          component="span"
                          aria-hidden="true"
                          title={label}
                          sx={{
                            mx: "auto",
                            height: 20,
                            width: 48,
                            borderRadius: "2px",
                            border: status === "absent" ? "1px solid" : 0,
                            borderColor: "divider",
                            bgcolor: cellColor,
                            color: cellTextColor,
                            display: "grid",
                            placeItems: "center",
                            fontSize: "0.75rem",
                            fontWeight: 800,
                            lineHeight: 1,
                          }}
                        >
                          {gridStatusSymbol(status)}
                        </Box>
                      );

                      return (
                        <Box component="td" key={run.build_id} sx={{ px: 0.5, py: 0.25 }}>
                          {status !== "absent" ? (
                            <Link
                              component={RouterLink}
                              to={jobRunPath(jobID, run.build_id)}
                              aria-label={label}
                              underline="none"
                              sx={{
                                display: "block",
                                borderRadius: "2px",
                                "&:focus-visible": {
                                  outline: "2px solid",
                                  outlineColor: "primary.main",
                                  outlineOffset: 2,
                                },
                              }}
                            >
                              {cell}
                            </Link>
                          ) : (
                            <Box component="span" role="img" aria-label={label} sx={{ display: "block" }}>
                              {cell}
                            </Box>
                          )}
                        </Box>
                      );
                    })}
                  </Box>
                ))}
              </Box>
            </Box>
          </Box>
        </Box>

        {hiddenRows > 0 && (
          <Box sx={{ px: 1.5, py: 1, borderTop: 1, borderColor: "divider", textAlign: "center" }}>
            <Button size="small" onClick={showMoreRows}>
              Show {Math.min(rowBatchSize, hiddenRows)} more tests
            </Button>
          </Box>
        )}
      </Panel>
    </>
  );
}
