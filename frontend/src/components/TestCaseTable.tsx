import { Fragment, useState } from "react";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import {
  Assignment,
  AutoAwesome,
  Cancel,
  CheckCircle,
  ChevronRight,
  Cloud,
  Dns,
  Inventory2,
  OpenInNew,
  Place,
  RemoveCircle,
} from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import type { TestCase } from "../types/dashboard";
import { testPath, testRunPath } from "../lib/routes";
import { formatDuration, fileToUrl, fileSortKey, highlightStackTrace } from "../lib/utils";
import { RichText } from "./RichText";
import { soft } from "../theme";
import { Panel } from "./Panel";
import { junitTestCases } from "../lib/buildFailures";

interface TestCaseTableProps {
  testCases: TestCase[];
  jobID?: string;
  buildId?: string;
  buildLogUrl?: string;
  webUrl?: string;
}

const statusOrder: Record<string, number> = {
  failed: 0,
  passed: 1,
  skipped: 2,
};

function statusIcon(status: string) {
  const iconSx = { fontSize: 20 };
  switch (status) {
    case "passed":
      return <CheckCircle color="success" sx={iconSx} />;
    case "failed":
      return <Cancel color="error" sx={iconSx} />;
    default:
      return <RemoveCircle color="disabled" sx={iconSx} />;
  }
}

// Hide Ginkgo setup/teardown entries unless they failed.
const setupPatterns = /synchronizedbeforesuite|synchronizedaftersuite|beforesuite|aftersuite/i;

function severityToColor(severity: string): "error" | "warning" | null {
  if (severity === "Critical" || severity === "High") return "error";
  if (severity === "Medium") return "warning";
  return null;
}

const externalLinkSx = {
  display: "inline-flex",
  alignItems: "center",
  gap: 0.5,
  color: "primary.main",
  fontSize: "0.75rem",
  textDecoration: "none",
  "&:hover": { textDecoration: "underline" },
};

export function TestCaseTable({ testCases, jobID, buildId, buildLogUrl, webUrl }: TestCaseTableProps) {
  const [expandedRows, setExpandedRows] = useState<Set<number>>(new Set());

  const filtered = junitTestCases(testCases).filter(
    (tc) => tc.status !== "skipped" && (tc.status === "failed" || !setupPatterns.test(tc.name)),
  );

  const sorted = [...filtered].sort(
    (a, b) => (statusOrder[a.status] ?? 3) - (statusOrder[b.status] ?? 3),
  );

  function toggleRow(idx: number) {
    setExpandedRows((prev) => {
      const next = new Set(prev);
      if (next.has(idx)) next.delete(idx);
      else next.add(idx);
      return next;
    });
  }

  return (
    <Panel sx={{ overflowX: "auto", borderRadius: 3 }}>
      <Box sx={{ minWidth: 0 }}>
        <Box
          sx={{
            display: "grid",
            gridTemplateColumns: { xs: "40px minmax(0, 1fr)", sm: "40px minmax(0, 1fr) 116px" },
            borderBottom: 1,
            borderColor: "divider",
            bgcolor: (t) => (t.vars ?? t).palette.surface.container,
          }}
        >
          <Box sx={{ px: 1.5, py: 1.25 }} />
          <Typography
            variant="label"
            component="div"
            sx={{
              px: 1.5,
              py: 1.25,
              color: "text.secondary",
              textTransform: "uppercase",
              letterSpacing: "0.08em",
            }}
          >
            Test Name
          </Typography>
          <Typography
            variant="label"
            component="div"
            sx={{
              display: { xs: "none", sm: "block" },
              pl: 1.5,
              pr: 2.5,
              py: 1.25,
              color: "text.secondary",
              textAlign: "right",
              textTransform: "uppercase",
              letterSpacing: "0.08em",
            }}
          >
            Duration
          </Typography>
        </Box>

        {sorted.map((tc, idx) => {
          const isExpanded = expandedRows.has(idx);
          const hasFail = tc.status === "failed" && Boolean(tc.failure_message);
          const stripeBg = idx % 2 === 0 ? "surface.container" : "surface.containerHigh";
          const aiFileCtx = {
            buildLogUrl,
            clusterArtifacts: tc.cluster_artifacts,
            webUrl,
            fileLinks: tc.ai_analysis?.file_links,
          };

          return (
            <Fragment key={idx}>
              <Box
                role={hasFail ? "button" : undefined}
                tabIndex={hasFail ? 0 : undefined}
                onClick={() => hasFail && toggleRow(idx)}
                onKeyDown={(e) => {
                  if (hasFail && (e.key === "Enter" || e.key === " ")) {
                    e.preventDefault();
                    toggleRow(idx);
                  }
                }}
                sx={{
                  display: "grid",
                  gridTemplateColumns: { xs: "40px minmax(0, 1fr)", sm: "40px minmax(0, 1fr) 116px" },
                  alignItems: "center",
                  bgcolor: stripeBg,
                  cursor: hasFail ? "pointer" : "default",
                  transition: (t) => t.transitions.create("background-color"),
                  ...(hasFail && {
                    "&:hover": { bgcolor: "surface.containerHighest" },
                    "&:focus-visible": {
                      outline: "2px solid",
                      outlineColor: "primary.main",
                      outlineOffset: -2,
                    },
                  }),
                }}
              >
                <Box sx={{ width: 40, px: { xs: 1, sm: 1.5 }, py: 1 }}>
                  {statusIcon(tc.status)}
                </Box>
                <Box
                  sx={{
                    minWidth: 0,
                    px: { xs: 1, sm: 1.5 },
                    py: 1,
                    color: "text.primary",
                    overflowWrap: "anywhere",
                  }}
                >
                  {jobID && tc.status === "failed" ? (
                    <Link
                      component={RouterLink}
                      to={buildId
                        ? testRunPath(jobID, tc.name, buildId)
                        : testPath(jobID, tc.name)}
                      underline="none"
                      onClick={(e) => e.stopPropagation()}
                      sx={{ color: "inherit", "&:hover": { color: "primary.main" } }}
                    >
                      {tc.name}
                    </Link>
                  ) : (
                    tc.name
                  )}
                  {tc.failure_location_url && (
                    <Link
                      href={tc.failure_location_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      onClick={(e) => e.stopPropagation()}
                      title="View source on GitHub"
                      aria-label="View source on GitHub"
                      sx={{ ml: 1, display: "inline-flex", color: "primary.main", verticalAlign: "text-bottom" }}
                    >
                      <OpenInNew sx={{ fontSize: 14 }} />
                    </Link>
                  )}
                  {tc.source === "build" && (
                    <Chip
                      size="small"
                      label="Build failure"
                      sx={{ ml: 1, height: 20, fontSize: "0.625rem", verticalAlign: "middle" }}
                    />
                  )}
                </Box>
                <Typography
                  variant="label"
                  component="div"
                  sx={{
                    display: { xs: "none", sm: "block" },
                    pl: 1.5,
                    pr: 2.5,
                    py: 1,
                    textAlign: "right",
                    color: "text.secondary",
                  }}
                >
                  {formatDuration(tc.duration_seconds)}
                </Typography>
              </Box>

              {tc.ai_summary && (
                <Box
                  sx={{
                    display: "flex",
                    alignItems: "flex-start",
                    gap: 1,
                    pl: { xs: 5, sm: 8 },
                    pr: { xs: 2, sm: 3 },
                    py: 0.75,
                    bgcolor: stripeBg,
                  }}
                >
                  <AutoAwesome sx={{ fontSize: 16, flexShrink: 0, color: "primary.main" }} />
                  <Typography
                    variant="caption"
                    sx={{ color: tc.ai_summary.is_transient ? "text.secondary" : "warning.main" }}
                  >
                    <RichText text={tc.ai_summary.summary} fileCtx={aiFileCtx} />
                    {tc.ai_summary.is_transient && (
                      <Box component="span" sx={{ ml: 0.5, color: "text.disabled" }}>
                        · Likely transient
                      </Box>
                    )}
                  </Typography>
                </Box>
              )}

              {hasFail && (
                <Collapse key={isExpanded ? "expanded" : "collapsed"} in={isExpanded} timeout="auto" unmountOnExit>
                  <Box
                    sx={{
                      borderTop: 1,
                      borderColor: "divider",
                      bgcolor: (t) => soft(t, "error", 0.05),
                      px: { xs: 2, sm: 3 },
                      py: 2,
                      display: "flex",
                      flexDirection: "column",
                      gap: 1.5,
                    }}
                  >
                    <Box
                      component="pre"
                      sx={{
                        m: 0,
                        p: 2,
                        borderRadius: 2,
                        bgcolor: (t) => soft(t, "error", 0.08),
                        color: "error.main",
                        fontFamily: "monospace",
                        fontSize: "0.75rem",
                        lineHeight: 1.6,
                        whiteSpace: "pre-wrap",
                        overflowX: "auto",
                      }}
                    >
                      {tc.failure_message}
                    </Box>

                    {tc.failure_body && (
                      <Accordion
                        disableGutters
                        elevation={0}
                        sx={{
                          bgcolor: "transparent",
                          border: 1,
                          borderColor: "divider",
                          borderRadius: 2,
                          "&:before": { display: "none" },
                        }}
                      >
                        <AccordionSummary
                          expandIcon={<ChevronRight sx={{ fontSize: 18 }} />}
                          sx={{
                            minHeight: 36,
                            "& .MuiAccordionSummary-content": { my: 0.75 },
                            "& .MuiAccordionSummary-expandIconWrapper.Mui-expanded": {
                              transform: "rotate(90deg)",
                            },
                          }}
                        >
                          <Typography variant="label" color="text.secondary">
                            Stack Trace
                          </Typography>
                        </AccordionSummary>
                        <AccordionDetails sx={{ pt: 0 }}>
                          <Box
                            component="pre"
                            sx={{
                              m: 0,
                              color: "text.secondary",
                              fontFamily: "monospace",
                              fontSize: "0.75rem",
                              lineHeight: 1.6,
                              whiteSpace: "pre-wrap",
                              overflowX: "auto",
                            }}
                          >
                            {highlightStackTrace(tc.failure_body)}
                          </Box>
                        </AccordionDetails>
                      </Accordion>
                    )}

                    {tc.failure_location && (
                      <Box sx={{ display: "flex", alignItems: "center", gap: 1, fontSize: "0.75rem" }}>
                        <Place sx={{ fontSize: 16, color: "text.secondary" }} />
                        {tc.failure_location_url ? (
                          <Link
                            href={tc.failure_location_url}
                            target="_blank"
                            rel="noopener noreferrer"
                            onClick={(e) => e.stopPropagation()}
                            sx={{ fontFamily: "monospace", color: "primary.main" }}
                          >
                            {tc.failure_location}
                          </Link>
                        ) : (
                          <Typography variant="caption" sx={{ fontFamily: "monospace", color: "text.secondary" }}>
                            {tc.failure_location}
                          </Typography>
                        )}
                      </Box>
                    )}

                    {tc.cluster_artifacts && (
                      <Panel
                        sx={{
                          borderRadius: 2,
                          p: 1.5,
                          bgcolor: (t) => (t.vars ?? t).palette.surface.container,
                          display: "flex",
                          flexDirection: "column",
                          gap: 1,
                        }}
                      >
                        <Typography variant="label" color="text.primary" sx={{ fontWeight: 700 }}>
                          Debug Artifacts — {tc.cluster_artifacts.cluster_name}
                        </Typography>

                        <Box sx={{ display: "flex", flexWrap: "wrap", columnGap: 2, rowGap: 0.75 }}>
                          {tc.cluster_artifacts.provider_activity_log && (
                            <Link
                              href={tc.cluster_artifacts.provider_activity_log}
                              target="_blank"
                              rel="noopener noreferrer"
                              onClick={(e) => e.stopPropagation()}
                              sx={externalLinkSx}
                            >
                              <Cloud sx={{ fontSize: 16 }} /> Provider Activity Log
                            </Link>
                          )}
                          {tc.cluster_artifacts.bootstrap_resources_url && (
                            <Link
                              href={tc.cluster_artifacts.bootstrap_resources_url}
                              target="_blank"
                              rel="noopener noreferrer"
                              onClick={(e) => e.stopPropagation()}
                              sx={externalLinkSx}
                            >
                              <Assignment sx={{ fontSize: 16 }} /> Cluster Resources
                            </Link>
                          )}
                          {tc.cluster_artifacts.pod_log_dirs &&
                            Object.entries(tc.cluster_artifacts.pod_log_dirs).map(([dir, url]) => (
                              <Link
                                key={dir}
                                href={url}
                                target="_blank"
                                rel="noopener noreferrer"
                                onClick={(e) => e.stopPropagation()}
                                sx={externalLinkSx}
                              >
                                <Inventory2 sx={{ fontSize: 16 }} /> {dir}
                              </Link>
                            ))}
                          {webUrl && (
                            <Link
                              href={`${webUrl}artifacts/clusters/bootstrap/logs/`}
                              target="_blank"
                              rel="noopener noreferrer"
                              onClick={(e) => e.stopPropagation()}
                              sx={externalLinkSx}
                            >
                              <Dns sx={{ fontSize: 16 }} /> Controller Logs
                            </Link>
                          )}
                        </Box>

                        {tc.cluster_artifacts.machines && tc.cluster_artifacts.machines.length > 0 && (
                          <Accordion
                            disableGutters
                            elevation={0}
                            sx={{
                              bgcolor: "transparent",
                              boxShadow: "none",
                              "&:before": { display: "none" },
                            }}
                          >
                            <AccordionSummary
                              expandIcon={<ChevronRight sx={{ fontSize: 16 }} />}
                              sx={{
                                minHeight: 32,
                                px: 0,
                                "& .MuiAccordionSummary-content": { my: 0.5 },
                                "& .MuiAccordionSummary-expandIconWrapper.Mui-expanded": {
                                  transform: "rotate(90deg)",
                                },
                              }}
                            >
                              <Box sx={{ display: "inline-flex", alignItems: "center", gap: 0.5 }}>
                                <Dns sx={{ fontSize: 16, color: "text.secondary" }} />
                                <Typography variant="label" color="text.secondary">
                                  Machine Logs ({tc.cluster_artifacts.machines.length} machines)
                                </Typography>
                              </Box>
                            </AccordionSummary>
                            <AccordionDetails sx={{ pt: 0, px: 0 }}>
                              <Box sx={{ display: "flex", flexDirection: "column", gap: 1 }}>
                                {tc.cluster_artifacts.machines.map((m) => (
                                  <Box key={m.name} sx={{ pl: 2 }}>
                                    <Typography variant="caption" sx={{ fontFamily: "monospace", color: "text.secondary" }}>
                                      {m.name}
                                    </Typography>
                                    <Box sx={{ mt: 0.5, display: "flex", flexWrap: "wrap", columnGap: 1.5, rowGap: 0.5 }}>
                                      {Object.entries(m.logs).map(([logType, url]) => (
                                        <Link
                                          key={logType}
                                          href={url}
                                          target="_blank"
                                          rel="noopener noreferrer"
                                          onClick={(e) => e.stopPropagation()}
                                          sx={{ ...externalLinkSx, fontSize: "0.6875rem" }}
                                        >
                                          {logType}
                                        </Link>
                                      ))}
                                    </Box>
                                  </Box>
                                ))}
                              </Box>
                            </AccordionDetails>
                          </Accordion>
                        )}
                      </Panel>
                    )}

                    {tc.ai_analysis && (
                      <Panel
                        sx={{
                          borderRadius: 2,
                          border: 1,
                          borderColor: (t) => soft(t, "primary", 0.3),
                          bgcolor: (t) => soft(t, "primary", 0.05),
                          p: { xs: 2, sm: 2.5 },
                          display: "flex",
                          flexDirection: "column",
                          gap: 2,
                        }}
                      >
                        <Box sx={{ display: "flex", alignItems: "center", gap: 1, flexWrap: "wrap" }}>
                          <AutoAwesome sx={{ fontSize: 20, color: "primary.main" }} />
                          <Typography variant="label" sx={{ color: "primary.main", fontWeight: 700 }}>
                            AI Analysis
                          </Typography>
                          {(() => {
                            const severityColor = severityToColor(tc.ai_analysis.severity);
                            return (
                              <Chip
                                size="small"
                                label={`Severity: ${tc.ai_analysis.severity}`}
                                sx={
                                  severityColor
                                    ? {
                                        bgcolor: (t) => soft(t, severityColor, 0.15),
                                        color: `${severityColor}.main`,
                                        fontWeight: 600,
                                      }
                                    : {
                                        bgcolor: "action.selected",
                                        color: "text.secondary",
                                        fontWeight: 600,
                                      }
                                }
                              />
                            );
                          })()}
                        </Box>
                        <Box>
                          <Typography variant="label" color="text.secondary" sx={{ mb: 0.5, fontWeight: 700 }}>
                            Root Cause
                          </Typography>
                          <Typography variant="body2" sx={{ color: "text.primary", lineHeight: 1.7, whiteSpace: "pre-line" }}>
                            <RichText text={tc.ai_analysis.root_cause} steps fileCtx={aiFileCtx} />
                          </Typography>
                        </Box>
                        <Box>
                          <Typography variant="label" color="text.secondary" sx={{ mb: 0.5, fontWeight: 700 }}>
                            Suggested Fix
                          </Typography>
                          <Typography variant="body2" sx={{ color: "text.primary", lineHeight: 1.7, whiteSpace: "pre-line" }}>
                            <RichText text={tc.ai_analysis.suggested_fix} steps fileCtx={aiFileCtx} />
                          </Typography>
                        </Box>
                        {tc.ai_analysis.relevant_files && tc.ai_analysis.relevant_files.length > 0 && (
                          <Box>
                            <Typography variant="label" color="text.secondary" sx={{ mb: 0.5, fontWeight: 700 }}>
                              Files to Check
                            </Typography>
                            <Box component="ul" sx={{ m: 0, pl: 2.5, color: "text.primary" }}>
                              {[...tc.ai_analysis.relevant_files]
                                .sort((a, b) => fileSortKey(a, aiFileCtx) - fileSortKey(b, aiFileCtx))
                                .map((f, i) => {
                                  const url = fileToUrl(f, aiFileCtx);
                                  return (
                                    <Box component="li" key={i} sx={{ fontFamily: "monospace", fontSize: "0.75rem", py: 0.25 }}>
                                      {url ? (
                                        <Link
                                          href={url}
                                          target="_blank"
                                          rel="noopener noreferrer"
                                          onClick={(e) => e.stopPropagation()}
                                          sx={{ color: "primary.main" }}
                                        >
                                          {f}
                                        </Link>
                                      ) : (
                                        <Box component="span" sx={{ color: "text.secondary" }}>
                                          {f}
                                        </Box>
                                      )}
                                    </Box>
                                  );
                                })}
                            </Box>
                          </Box>
                        )}
                      </Panel>
                    )}
                  </Box>
                </Collapse>
              )}
            </Fragment>
          );
        })}
      </Box>
    </Panel>
  );
}
