import { Fragment, useState } from "react";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import ButtonBase from "@mui/material/ButtonBase";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import {
  Assignment,
  AutoAwesome,
  ChevronRight,
  Cloud,
  Dns,
  Inventory2,
  Place,
} from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import type { TestCase } from "../types/dashboard";
import { testPath, testRunPath } from "../lib/routes";
import { formatDuration, highlightStackTrace } from "../lib/utils";
import { RichText } from "./RichText";
import { soft } from "../theme";
import { AiAnalysisPanel } from "./AiAnalysisPanel";
import { parseTestDisplayName } from "../lib/detailTitles";
import { overviewTypography } from "../theme/overview";
import { hasInlineTestEvidence } from "../lib/jobDetail";

interface TestCaseTableProps {
  testCases: TestCase[];
  jobID?: string;
  buildId?: string;
  buildLogUrl?: string;
  webUrl?: string;
}

function testStatusPresentation(status: TestCase["status"]) {
  switch (status) {
    case "passed":
      return { label: "Passed", color: "success.main" } as const;
    case "failed":
      return { label: "Failed", color: "error.main" } as const;
    default:
      return { label: "Skipped", color: "text.disabled" } as const;
  }
}

export function EvidenceSourceLink({
  href,
  label,
  text,
}: {
  href: string;
  label: string;
  text: string;
}) {
  return (
    <Link
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={label}
      sx={{
        minHeight: 44,
        display: "inline-flex",
        alignItems: "center",
        px: 0.5,
        borderRadius: "4px",
        fontFamily: overviewTypography.data.fontFamily,
        fontSize: "13px",
        lineHeight: "20px",
        color: "primary.main",
        overflowWrap: "anywhere",
        "&:focus-visible": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: 2,
        },
      }}
    >
      {text}
    </Link>
  );
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

  function toggleRow(idx: number) {
    setExpandedRows((prev) => {
      const next = new Set(prev);
      if (next.has(idx)) next.delete(idx);
      else next.add(idx);
      return next;
    });
  }

  return (
    <Box sx={{ overflowX: "clip", bgcolor: "surface.container" }}>
      <Box sx={{ minWidth: 0 }}>
        <Box
          sx={{
            display: { xs: "none", md: "grid" },
            gridTemplateColumns: "110px minmax(0, 1fr) 90px 110px 90px",
            alignItems: "center",
            minHeight: 42,
            borderBottom: "1px solid",
            borderColor: "divider",
            bgcolor: "surface.containerHigh",
          }}
        >
          <Typography component="div" color="textSecondary" sx={{ px: 1.5, ...overviewTypography.tableHeading }}>
            Status
          </Typography>
          <Typography component="div" color="textSecondary" sx={{ px: 1.5, ...overviewTypography.tableHeading }}>
            Test name
          </Typography>
          <Typography component="div" color="textSecondary" sx={{ px: 1.5, textAlign: "right", ...overviewTypography.tableHeading }}>
            Duration
          </Typography>
          <Typography component="div" color="textSecondary" sx={{ px: 1.5, ...overviewTypography.tableHeading }}>
            Analysis
          </Typography>
          <Typography
            component="div"
            color="textSecondary"
            sx={{ px: 0.5, textAlign: "center", ...overviewTypography.tableHeading }}
          >
            Evidence
          </Typography>
        </Box>

        {testCases.map((tc, idx) => {
          const isExpanded = expandedRows.has(idx);
          const hasEvidence = hasInlineTestEvidence(tc);
          const stripeBg = idx % 2 === 0 ? "surface.container" : "surface.containerHigh";
          const displayName = parseTestDisplayName(tc.name).displayName;
          const status = testStatusPresentation(tc.status);
          const duration = formatDuration(tc.duration_seconds);
          const analysisPath = jobID
            ? buildId
              ? testRunPath(jobID, tc.name, buildId)
              : testPath(jobID, tc.name)
            : null;
          const aiFileCtx = {
            buildLogUrl,
            clusterArtifacts: tc.cluster_artifacts,
            webUrl,
            fileLinks: tc.ai_analysis?.file_links,
          };

          return (
            <Fragment key={idx}>
              <Box
                sx={{
                  display: "grid",
                  gridTemplateColumns: {
                    xs: "minmax(0, 1fr)",
                    md: "110px minmax(0, 1fr) 90px 110px 90px",
                  },
                  alignItems: "stretch",
                  minHeight: 54,
                  bgcolor: stripeBg,
                  borderBottomWidth: tc.ai_summary ? 0 : "1px",
                  borderBottomStyle: "solid",
                  borderBottomColor: "divider",
                }}
              >
                {analysisPath ? (
                  <Link
                    component={RouterLink}
                    to={analysisPath}
                    underline="none"
                    aria-label={`Open analysis for ${displayName}. ${status.label}. Duration ${duration}`}
                    sx={{
                      gridColumn: { xs: "1", md: "1 / 5" },
                      display: "grid",
                      gridTemplateColumns: {
                        xs: "minmax(0, 1fr) auto",
                        md: "110px minmax(0, 1fr) 90px 110px",
                      },
                      gridTemplateAreas: {
                        xs: '"name analysis" "status duration"',
                        md: '"status name duration analysis"',
                      },
                      minWidth: 0,
                      minHeight: 54,
                      alignItems: "center",
                      color: "inherit",
                      cursor: "pointer",
                      transition: (theme) =>
                        theme.transitions.create("background-color", {
                          duration: theme.transitions.duration.shortest,
                        }),
                      "&:hover": {
                        bgcolor: "surface.containerHighest",
                        textDecoration: "none",
                      },
                      "&.Mui-focusVisible": {
                        outline: "2px solid",
                        outlineColor: "primary.main",
                        outlineOffset: -2,
                      },
                    }}
                  >
                    <Box
                      sx={{
                        gridArea: "status",
                        minWidth: 0,
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 0.75,
                        px: 1.5,
                        py: { xs: 0.5, sm: 1 },
                        color: status.color,
                      }}
                    >
                      <Box
                        component="span"
                        sx={{
                          width: 7,
                          height: 7,
                          borderRadius: "2px",
                          bgcolor: "currentColor",
                          flexShrink: 0,
                        }}
                      />
                      <Typography
                        component="span"
                        sx={{ fontSize: "13px", lineHeight: "18px", fontWeight: 700 }}
                      >
                        {status.label}
                      </Typography>
                    </Box>
                    <Box
                      sx={{
                        gridArea: "name",
                        minWidth: 0,
                        px: 1.5,
                        py: 1,
                        color: "text.primary",
                        overflowWrap: "anywhere",
                      }}
                    >
                      <Typography
                        component="span"
                        title={tc.name}
                        sx={{ fontSize: "14px", lineHeight: "20px", fontWeight: 650 }}
                      >
                        {displayName}
                      </Typography>
                      {tc.source === "build" && (
                        <Chip
                          size="small"
                          label="Build failure"
                          sx={{
                            ml: 1,
                            height: 20,
                            borderRadius: "4px",
                            fontSize: "0.625rem",
                            verticalAlign: "middle",
                          }}
                        />
                      )}
                    </Box>
                    <Typography
                      component="div"
                      color="textSecondary"
                      sx={{
                        gridArea: "duration",
                        px: 1.5,
                        py: { xs: 0.5, sm: 1 },
                        textAlign: "right",
                        ...overviewTypography.data,
                      }}
                    >
                      {duration}
                    </Typography>
                    <Typography
                      component="span"
                      color="primary"
                      sx={{
                        gridArea: "analysis",
                        minHeight: 44,
                        display: "inline-flex",
                        alignItems: "center",
                        justifyContent: { xs: "flex-end", md: "flex-start" },
                        px: { xs: 0.75, sm: 1.5 },
                        fontSize: "13px",
                        fontWeight: 700,
                        whiteSpace: "nowrap",
                      }}
                    >
                      Analysis →
                    </Typography>
                  </Link>
                ) : (
                  <Box sx={{ gridColumn: { xs: "1", md: "1 / 5" }, minHeight: 54 }} />
                )}
                <Box
                  sx={{
                    gridColumn: { xs: "1", md: "5" },
                    gridRow: { xs: "2", md: "1" },
                    minHeight: 44,
                    display: "flex",
                    alignItems: "center",
                    justifyContent: { xs: "flex-end", md: "center" },
                    gap: 0.25,
                    px: 0.5,
                    borderTopWidth: { xs: "1px", md: 0 },
                    borderTopStyle: "solid",
                    borderTopColor: "divider",
                  }}
                >
                  {hasEvidence && (
                    <ButtonBase
                      type="button"
                      onClick={() => toggleRow(idx)}
                      aria-label={isExpanded ? `Hide inline evidence for ${tc.name}` : `Show inline evidence for ${tc.name}`}
                      aria-expanded={isExpanded}
                      aria-controls={`test-result-details-${idx}`}
                      sx={{
                        width: "100%",
                        minWidth: 0,
                        minHeight: 44,
                        justifyContent: "center",
                        gap: 0.5,
                        px: 0.75,
                        borderRadius: "4px",
                        color: "text.secondary",
                        fontSize: "12px",
                        fontWeight: 650,
                        "&:hover": {
                          bgcolor: "surface.containerHighest",
                          color: "text.primary",
                        },
                        "&.Mui-focusVisible": {
                          outline: "2px solid",
                          outlineColor: "primary.main",
                          outlineOffset: -2,
                        },
                      }}
                    >
                      Evidence
                      <ChevronRight
                        sx={{
                          fontSize: 18,
                          transform: isExpanded ? "rotate(90deg)" : "rotate(0deg)",
                          transition: (theme) =>
                            theme.transitions.create("transform", {
                              duration: theme.transitions.duration.shortest,
                            }),
                          "@media (prefers-reduced-motion: reduce)": { transition: "none" },
                        }}
                      />
                    </ButtonBase>
                  )}
                </Box>
              </Box>

              {tc.ai_summary && (
                <Box
                  sx={{
                    display: "flex",
                    alignItems: "flex-start",
                    gap: 1,
                    px: 1.5,
                    py: 0.75,
                    bgcolor: stripeBg,
                    borderBottom: "1px solid",
                    borderColor: "divider",
                  }}
                >
                  <AutoAwesome sx={{ fontSize: 16, flexShrink: 0, color: "primary.main" }} />
                  <Typography
                    component="div"
                    sx={{
                      color: "text.primary",
                      fontSize: "13.5px",
                      lineHeight: "20px",
                      fontWeight: 450,
                    }}
                  >
                    <RichText text={tc.ai_summary.summary} fileCtx={aiFileCtx} />
                    {tc.ai_summary.is_transient && (
                      <Box
                        component="span"
                        sx={{ ml: 0.75, color: "text.secondary", fontSize: "13px" }}
                      >
                        · Likely transient
                      </Box>
                    )}
                  </Typography>
                </Box>
              )}

              {hasEvidence && (
                <Collapse key={isExpanded ? "expanded" : "collapsed"} in={isExpanded} timeout="auto" unmountOnExit>
                  <Box
                    id={`test-result-details-${idx}`}
                    sx={{
                      borderTop: 1,
                      borderColor: "divider",
                      bgcolor: "surface.containerHigh",
                      px: { xs: 2, sm: 3 },
                      py: 2,
                      display: "flex",
                      flexDirection: "column",
                      gap: 1.5,
                    }}
                  >
                    {tc.failure_message && (
                      <Box
                        component="pre"
                        sx={{
                          m: 0,
                          p: 2,
                          borderRadius: "4px",
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
                    )}

                    {tc.failure_body && (
                      <Accordion
                        disableGutters
                        elevation={0}
                        sx={{
                          bgcolor: "transparent",
                          border: 1,
                          borderColor: "divider",
                          borderRadius: "4px",
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
                          <Typography variant="label" color="textSecondary">
                            Stack trace
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
                          <EvidenceSourceLink
                            href={tc.failure_location_url}
                            label={`View source for ${displayName} on GitHub`}
                            text={tc.failure_location}
                          />
                        ) : (
                          <Typography variant="caption" sx={{ fontFamily: "monospace", color: "text.secondary" }}>
                            {tc.failure_location}
                          </Typography>
                        )}
                      </Box>
                    )}

                    {tc.cluster_artifacts && (
                      <Box
                        sx={{
                          pt: 1.5,
                          borderTop: "1px solid",
                          borderColor: "divider",
                          display: "flex",
                          flexDirection: "column",
                          gap: 1,
                        }}
                      >
                        <Typography variant="label" color="textPrimary" sx={{ fontWeight: 700 }}>
                          Debug artifacts: {tc.cluster_artifacts.cluster_name}
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
                              <Cloud sx={{ fontSize: 16 }} /> Provider activity log
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
                              <Assignment sx={{ fontSize: 16 }} /> Cluster resources
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
                              <Dns sx={{ fontSize: 16 }} /> Controller logs
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
                                <Typography variant="label" color="textSecondary">
                                  Machine logs ({tc.cluster_artifacts.machines.length} machines)
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
                      </Box>
                    )}

                    {tc.ai_analysis && (
                      <Box
                        sx={{
                          pt: 1.5,
                          borderTop: "1px solid",
                          borderColor: "divider",
                        }}
                      >
                        <AiAnalysisPanel
                          analysis={tc.ai_analysis}
                          fileCtx={aiFileCtx}
                          buildWebURL={webUrl}
                          appearance="detail"
                        />
                      </Box>
                    )}
                  </Box>
                </Collapse>
              )}
            </Fragment>
          );
        })}
      </Box>
    </Box>
  );
}
