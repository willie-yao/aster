import { useId, useState, type ReactNode } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import OpenInNew from "@mui/icons-material/OpenInNew";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import {
  analysisTraceEventDetails,
  formatTraceDuration,
  traceStatusLabel,
  traceTone,
  type TraceTone,
} from "../lib/analysisTraces";
import type { AnalysisTrace, AnalysisTraceEvent } from "../types/traces";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";

export interface AnalysisTraceLedgerItem {
  trace: AnalysisTrace;
  displayTitle: string;
  displayJob: string;
  testHref: string;
  responseIDs: string[];
}

const toneColor: Record<TraceTone, "success.main" | "warning.main" | "error.main" | "text.secondary"> = {
  success: "success.main",
  warning: "warning.main",
  error: "error.main",
  neutral: "text.secondary",
};

export function TraceStatusSignal({ value }: { value?: string }) {
  const tone = traceTone(value);
  return (
    <Box
      component="span"
      sx={{
        minWidth: 0,
        display: "inline-flex",
        alignItems: "center",
        gap: 0.75,
        color: toneColor[tone],
        fontSize: "13px",
        lineHeight: "19px",
        fontWeight: 700,
      }}
    >
      <Box component="span" aria-hidden="true" sx={{ width: 8, height: 8, borderRadius: "2px", bgcolor: "currentColor", flexShrink: 0 }} />
      {traceStatusLabel(value)}
    </Box>
  );
}

export function TraceNotice({
  severity,
  title,
  children,
  role,
}: {
  severity: "info" | "warning" | "error";
  title: string;
  children?: ReactNode;
  role?: "alert" | "status";
}) {
  const color = severity === "error" ? "error.main" : severity === "warning" ? "warning.main" : "primary.main";
  return (
    <Box
      role={role ?? (severity === "error" ? "alert" : "status")}
      sx={{
        minHeight: 54,
        display: "grid",
        gridTemplateColumns: "12px minmax(0, 1fr)",
        alignItems: "start",
        gap: 1.5,
        px: 1.5,
        py: 1.25,
        bgcolor: "surface.container",
        borderBlock: "1px solid",
        borderColor: "divider",
        boxShadow: `inset 3px 0 0 var(--mui-palette-${severity === "error" ? "error" : severity === "warning" ? "warning" : "primary"}-main)`,
      }}
    >
      <Box aria-hidden="true" sx={{ width: 8, height: 8, mt: 0.75, borderRadius: "2px", bgcolor: color }} />
      <Box sx={{ minWidth: 0 }}>
        <Typography sx={{ fontSize: "14px", lineHeight: "20px", fontWeight: 700 }}>{title}</Typography>
        {children && (
          <Box sx={{ mt: 0.25, color: "text.secondary", ...overviewTypography.description, overflowWrap: "anywhere" }}>
            {children}
          </Box>
        )}
      </Box>
    </Box>
  );
}

function CopyIdentifier({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    if (!navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  }

  return (
    <Box
      sx={{
        minWidth: 0,
        minHeight: { xs: 44, md: 36 },
        display: "inline-flex",
        alignItems: "center",
        gap: 0.5,
        pl: 1,
        border: "1px solid",
        borderColor: "divider",
        borderRadius: "4px",
        bgcolor: "surface.container",
      }}
    >
      <Typography component="code" title={value} sx={{ ...overviewTypography.data, color: "text.secondary", overflowWrap: "anywhere" }}>
        {label} {value}
      </Typography>
      <Button
        type="button"
        onClick={() => void copy()}
        aria-label={`Copy ${label.toLowerCase()} ${value}`}
        sx={{
          alignSelf: "stretch",
          minWidth: { xs: 44, md: 36 },
          minHeight: { xs: 44, md: 34 },
          px: 1,
          borderInlineStart: "1px solid",
          borderInlineStartColor: "divider",
          borderRadius: 0,
          color: "text.secondary",
          fontSize: "12px",
        }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
    </Box>
  );
}

function TraceEventOutcome({ outcome }: { outcome?: string }) {
  if (!outcome) {
    return (
      <Typography component="span" color="text.secondary" sx={overviewTypography.data} aria-label="No outcome reported">
        Not reported
      </Typography>
    );
  }
  return <TraceStatusSignal value={outcome} />;
}

export function TraceEventRow({ event }: { event: AnalysisTraceEvent }) {
  const details = analysisTraceEventDetails(event);
  return (
    <Box
      role="row"
      sx={{
        minWidth: 0,
        display: "grid",
        gridTemplateColumns: { xs: "64px minmax(0, 1fr)", md: "54px 88px 178px 122px minmax(0, 1fr)" },
        gridTemplateAreas: {
          xs: '"sequence kind" "elapsed outcome" "details details"',
          md: '"sequence elapsed kind outcome details"',
        },
        columnGap: 1.5,
        rowGap: 0.25,
        alignItems: "start",
        px: 1.5,
        py: 1.1,
        borderBottom: "1px solid",
        borderColor: "divider",
        "&:last-child": { borderBottom: 0 },
      }}
    >
      <Typography role="cell" sx={{ gridArea: "sequence", color: "text.secondary", ...overviewTypography.data }}>
        {String(event.sequence).padStart(2, "0")}
      </Typography>
      <Typography role="cell" sx={{ gridArea: "elapsed", color: "text.secondary", ...overviewTypography.data }}>
        +{formatTraceDuration(event.elapsed_ms)}
      </Typography>
      <Typography role="cell" sx={{ gridArea: "kind", ...overviewTypography.data, color: "text.primary", fontWeight: 700, overflowWrap: "anywhere" }}>
        {event.kind}
      </Typography>
      <Box role="cell" sx={{ gridArea: "outcome", minWidth: 0 }}>
        <TraceEventOutcome outcome={event.outcome} />
      </Box>
      <Typography
        role="cell"
        sx={{
          gridArea: "details",
          mt: { xs: 0.75, md: 0 },
          color: "text.secondary",
          ...overviewTypography.data,
          lineHeight: { xs: "20px", md: overviewTypography.data.lineHeight },
          overflowWrap: "anywhere",
        }}
      >
        {details.join(" · ") || "No additional metadata"}
      </Typography>
    </Box>
  );
}

function TraceEventLedger({ events }: { events: AnalysisTraceEvent[] }) {
  return (
    <Box component="section" aria-label="Trace events" sx={{ bgcolor: "surface.container" }}>
      <DetailSectionBand title="Events" headingLevel="h3" metadata={`${events.length} recorded`} />
      <Box role="table" aria-label="Trace event ledger">
        <Box
          role="row"
          sx={{
            minHeight: 36,
            display: { xs: "none", md: "grid" },
            gridTemplateColumns: "54px 88px 178px 122px minmax(0, 1fr)",
            gap: 1.5,
            alignItems: "center",
            px: 1.5,
            borderBottom: "1px solid",
            borderColor: "divider",
          }}
        >
          {["Seq", "Elapsed", "Event kind", "Outcome", "Details"].map((label) => (
            <Typography key={label} role="columnheader" color="text.secondary" sx={overviewTypography.tableHeading}>
              {label}
            </Typography>
          ))}
        </Box>
        {events.map((event) => (
          <TraceEventRow key={`${event.sequence}-${event.kind}`} event={event} />
        ))}
      </Box>
    </Box>
  );
}

function traceSummaryLabel(item: AnalysisTraceLedgerItem, open: boolean): string {
  const trace = item.trace;
  return `${open ? "Collapse" : "Expand"} ${traceStatusLabel(trace.outcome)} trace for ${item.displayTitle}. Job ${item.displayJob}. Build ${trace.build_id}. API mode ${trace.api_mode}. ${formatTraceDuration(trace.elapsed_ms)}. ${trace.events.length} ${trace.events.length === 1 ? "event" : "events"}.`;
}

function TraceSummaryRow({ item }: { item: AnalysisTraceLedgerItem }) {
  const [open, setOpen] = useState(false);
  const generatedID = useId();
  const contentID = `analysis-trace-${generatedID.replaceAll(":", "")}`;
  const trace = item.trace;
  const traceNoticeTitle = trace.error_code && trace.truncated
    ? "Trace error and truncated recording"
    : trace.error_code
      ? "Trace error"
      : "Trace truncated";

  return (
    <Box component="article" sx={{ minWidth: 0, bgcolor: "surface.container", borderTop: "1px solid", borderColor: "divider" }}>
      <ButtonBase
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        aria-controls={contentID}
        aria-label={traceSummaryLabel(item, open)}
        sx={{
          width: "100%",
          minHeight: { xs: 44, md: 72 },
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr) 44px", md: "96px minmax(210px, 1.5fr) minmax(170px, 1fr) 152px 78px 58px 40px" },
          gridTemplateAreas: {
            xs: '"outcome chevron" "title chevron" "build chevron" "mobileMeta chevron"',
            md: '"outcome title build api duration events chevron"',
          },
          alignItems: "center",
          color: "text.primary",
          textAlign: "left",
          "&:hover": { bgcolor: "surface.containerHigh" },
          "&.Mui-focusVisible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: -2,
          },
        }}
      >
        <Box sx={{ gridArea: "outcome", minWidth: 0, px: { xs: 1.5, md: 1.25 }, pt: { xs: 1.25, md: 0 } }}>
          <TraceStatusSignal value={trace.outcome} />
        </Box>
        <Box sx={{ gridArea: "title", minWidth: 0, px: { xs: 1.5, md: 1.25 }, py: { xs: 0.5, md: 1.25 } }}>
          <Typography
            title={trace.test_name}
            sx={{
              display: "-webkit-box",
              WebkitBoxOrient: "vertical",
              WebkitLineClamp: 2,
              overflow: "hidden",
              fontSize: "15px",
              lineHeight: "21px",
              fontWeight: 680,
              overflowWrap: "anywhere",
            }}
          >
            {item.displayTitle}
          </Typography>
          <Typography title={trace.job_id} color="text.secondary" sx={{ mt: 0.25, ...overviewTypography.data, overflowWrap: "anywhere" }}>
            {item.displayJob}
          </Typography>
        </Box>
        <Typography sx={{ gridArea: "build", minWidth: 0, px: { xs: 1.5, md: 1.25 }, pb: { xs: 0.25, md: 0 }, ...overviewTypography.data, overflowWrap: "anywhere" }}>
          <Box component="span" sx={{ display: { xs: "inline", md: "none" } }}>Build </Box>
          {trace.build_id}
        </Typography>
        <Typography sx={{ gridArea: "api", display: { xs: "none", md: "block" }, minWidth: 0, px: 1.25, ...overviewTypography.data, overflowWrap: "anywhere" }}>
          {trace.api_mode}
        </Typography>
        <Typography sx={{ gridArea: "duration", display: { xs: "none", md: "block" }, minWidth: 0, px: 1.25, ...overviewTypography.data }}>
          {formatTraceDuration(trace.elapsed_ms)}
        </Typography>
        <Typography sx={{ gridArea: "events", display: { xs: "none", md: "block" }, minWidth: 0, px: 1.25, ...overviewTypography.data }}>
          {trace.events.length}
        </Typography>
        <Typography
          color="text.secondary"
          sx={{
            gridArea: "mobileMeta",
            display: { xs: "block", md: "none" },
            minWidth: 0,
            px: 1.5,
            pb: 1.25,
            ...overviewTypography.data,
            overflowWrap: "anywhere",
          }}
        >
          {trace.api_mode} · {formatTraceDuration(trace.elapsed_ms)} · {trace.events.length} {trace.events.length === 1 ? "event" : "events"}
        </Typography>
        <Box sx={{ gridArea: "chevron", alignSelf: "stretch", display: "grid", placeItems: "center", color: "text.secondary" }}>
          <ChevronRight
            aria-hidden="true"
            sx={{
              fontSize: 20,
              transform: open ? "rotate(90deg)" : "rotate(0deg)",
              transition: (theme) => theme.transitions.create("transform", { duration: theme.transitions.duration.shortest }),
              "@media (prefers-reduced-motion: reduce)": { transition: "none" },
            }}
          />
        </Box>
      </ButtonBase>

      <Collapse in={open} timeout="auto">
        <Box id={contentID} sx={{ minWidth: 0, bgcolor: "surface.containerLow", borderTop: "1px solid", borderColor: "divider" }}>
          <Stack
            direction="row"
            sx={{ minWidth: 0, minHeight: 56, alignItems: "center", gap: 1, flexWrap: "wrap", px: 1.5, py: 1 }}
          >
            <Button
              component={RouterLink}
              to={item.testHref}
              variant="outlined"
              endIcon={<OpenInNew sx={{ fontSize: 15 }} />}
              sx={{ minHeight: { xs: 44, md: 36 }, borderRadius: "4px" }}
            >
              Open test run
            </Button>
            <CopyIdentifier label="Build" value={trace.build_id} />
            {item.responseIDs.map((responseID, index) => (
              <CopyIdentifier
                key={responseID}
                label={item.responseIDs.length === 1 ? "Response" : `Response ${index + 1}`}
                value={responseID}
              />
            ))}
          </Stack>

          {(trace.error_code || trace.truncated) && (
            <Box sx={{ px: 1.5, pb: 1.5 }}>
              <TraceNotice severity={trace.error_code ? "error" : "warning"} title={traceNoticeTitle}>
                {trace.error_code && (
                  <>
                    Error code: <Box component="code" sx={overviewTypography.data}>{trace.error_code}</Box>
                    {trace.truncated ? ". " : ""}
                  </>
                )}
                {trace.truncated ? "The bounded recorder omitted later events." : null}
              </TraceNotice>
            </Box>
          )}

          <TraceEventLedger events={trace.events} />
        </Box>
      </Collapse>
    </Box>
  );
}

export function AnalysisTraceLedger({ items }: { items: AnalysisTraceLedgerItem[] }) {
  return (
    <Box component="section" aria-label="Trace ledger" sx={{ minWidth: 0, bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Trace ledger" metadata={`${items.length} ${items.length === 1 ? "trace" : "traces"}`} />
      <Box
        aria-hidden="true"
        sx={{
          minHeight: 38,
          display: { xs: "none", md: "grid" },
          gridTemplateColumns: "96px minmax(210px, 1.5fr) minmax(170px, 1fr) 152px 78px 58px 40px",
          alignItems: "center",
          px: 0,
          borderBottom: "1px solid",
          borderColor: "divider",
        }}
      >
        {["Outcome", "Test", "Build", "API mode", "Duration", "Events", ""].map((label, index) => (
          <Typography key={`${label}-${index}`} color="text.secondary" sx={{ px: 1.25, ...overviewTypography.tableHeading }}>
            {label}
          </Typography>
        ))}
      </Box>
      {items.map((item, index) => (
        <TraceSummaryRow key={`${item.trace.job_id}-${item.trace.build_id}-${item.trace.test_name}-${index}`} item={item} />
      ))}
    </Box>
  );
}
