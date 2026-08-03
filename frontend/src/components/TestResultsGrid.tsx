import { useMemo } from "react";
import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import type { BuildResult } from "../types/dashboard";
import { jobRunPath, testPath } from "../lib/routes";
import { shortDate, shortTestName } from "../lib/utils";
import { Panel } from "./Panel";
import { junitTestCases } from "../lib/buildFailures";

interface TestResultsGridProps {
  runs: BuildResult[];
  jobID: string;
}

type CellStatus = "passed" | "failed" | "skipped" | "absent";

interface GridRow {
  testName: string;
  failCount: number;
  cells: CellStatus[];
}

const setupPatterns =
  /^(SynchronizedBeforeSuite|SynchronizedAfterSuite|BeforeSuite|AfterSuite)$/i;

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

    const testMap = new Map<string, CellStatus[]>();

    for (let col = 0; col < sortedRuns.length; col++) {
      const run = sortedRuns[col];
      for (const tc of junitTestCases(run.test_cases)) {
        if (!testMap.has(tc.name)) {
          testMap.set(tc.name, new Array(sortedRuns.length).fill("skipped"));
        }
        testMap.get(tc.name)![col] = tc.status;
      }
    }

    const rows: GridRow[] = [];

    for (const [testName, cells] of testMap) {
      const failCount = cells.filter((s) => s === "failed").length;
      const hasPass = cells.some((s) => s === "passed");
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

  if (runs.length === 0 || gridRows.length === 0) {
    return (
      <Panel sx={{ borderRadius: 3, p: 3, textAlign: "center" }}>
        <Typography variant="body2" color="text.secondary">
          {runs.length === 0
            ? "No runs available."
            : "All tests passed across all runs — nothing to display."}
        </Typography>
      </Panel>
    );
  }

  return (
    <>
      <Panel
        sx={{
          display: { xs: "block", md: "none" },
          borderRadius: 3,
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
          borderRadius: 3,
          overflow: "hidden",
          bgcolor: (t) => (t.vars ?? t).palette.surface.main,
        }}
      >
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
                    sx={{
                      bgcolor: (t) => (t.vars ?? t).palette.surface.main,
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
                {gridRows.map((row) => (
                  <Box
                    component="tr"
                    key={row.testName}
                    sx={{
                      height: 28,
                      transition: (t) => t.transitions.create("background-color"),
                      "&:hover td": {
                        bgcolor: (t) => (t.vars ?? t).palette.surface.containerHigh,
                      },
                    }}
                  >
                    <Box component="td" sx={{ bgcolor: (t) => (t.vars ?? t).palette.surface.main, p: 0 }}>
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
                          transition: (t) => t.transitions.create("color"),
                          "&:hover": { color: "primary.main" },
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

          <Box sx={{ overflowX: "auto" }}>
            <Box component="table" sx={{ borderCollapse: "collapse" }}>
              <Box component="thead">
                <Box component="tr" sx={{ height: 32 }}>
                  {sortedRuns.map((run) => (
                    <Box
                      component="th"
                      key={run.build_id}
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
                {gridRows.map((row) => (
                  <Box
                    component="tr"
                    key={row.testName}
                    sx={{
                      height: 28,
                      transition: (t) => t.transitions.create("background-color"),
                      "&:hover td": {
                        bgcolor: (t) => (t.vars ?? t).palette.surface.containerHigh,
                      },
                    }}
                  >
                    {row.cells.map((status, colIdx) => {
                      const run = sortedRuns[colIdx];
                      const cellColor =
                        status === "passed"
                          ? "success.main"
                          : status === "failed"
                            ? "error.main"
                            : "action.disabledBackground";

                      const cell = (
                        <Box
                          title={`${shortTestName(row.testName)}\n#${run.build_id} — ${status}`}
                          sx={{
                            mx: "auto",
                            height: 20,
                            width: 48,
                            borderRadius: "2px",
                            bgcolor: cellColor,
                          }}
                        />
                      );

                      return (
                        <Box component="td" key={run.build_id} sx={{ px: 0.5, py: 0.25 }}>
                          {status !== "absent" ? (
                            <Link
                              component={RouterLink}
                              to={jobRunPath(jobID, run.build_id)}
                              underline="none"
                              sx={{ display: "block" }}
                            >
                              {cell}
                            </Link>
                          ) : (
                            cell
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
      </Panel>
    </>
  );
}
