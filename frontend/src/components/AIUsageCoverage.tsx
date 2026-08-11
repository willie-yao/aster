import { useId, useState, type ReactNode } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { coverageStateLabel, formatCoverage, pricedRequestCoverageNote } from "../lib/aiUsage";
import type { AIUsageReport } from "../types/usage";
import { DetailSectionBand } from "./DetailSectionBand";
import { TraceNotice } from "./AnalysisTraceLedger";
import { overviewTypography } from "../theme/overview";

function coverageStatusLabel(status: AIUsageReport["coverage"]["status"]): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
}

export function UsageCoverageStatus({ status }: { status: AIUsageReport["coverage"]["status"] }) {
  const color = status === "complete" ? "success.main" : status === "partial" ? "warning.main" : "text.secondary";
  return (
    <Box component="span" sx={{ display: "inline-flex", alignItems: "center", gap: 0.75, color, fontSize: "14px", lineHeight: "20px", fontWeight: 700 }}>
      <Box component="span" aria-hidden="true" sx={{ width: 8, height: 8, borderRadius: "2px", bgcolor: "currentColor" }} />
      {coverageStatusLabel(status)}
    </Box>
  );
}

function CoverageItem({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Box
      sx={{
        minWidth: 0,
        minHeight: 64,
        px: 1.5,
        py: 1.1,
        borderTop: "1px solid",
        borderColor: "divider",
        "&:not(:nth-of-type(3n + 1))": { borderInlineStart: { md: "1px solid" }, borderInlineStartColor: { md: "divider" } },
        "&:nth-of-type(even)": { borderInlineStart: { xs: "1px solid", md: "1px solid" }, borderInlineStartColor: "divider" },
      }}
    >
      <Typography color="text.secondary" sx={overviewTypography.tableHeading}>{label}</Typography>
      <Box sx={{ mt: 0.25, color: "text.primary", ...overviewTypography.data, overflowWrap: "anywhere" }}>{children}</Box>
    </Box>
  );
}

function cacheWriteCoverage(data: AIUsageReport): string {
  const reported = data.coverage.cache_write_reported_requests;
  const priced = data.coverage.cache_write_priced_requests;
  const unreported = data.coverage.cache_write_unreported_requests;
  if (reported === undefined && priced === undefined && unreported === undefined) return "Unavailable for legacy records";
  if ((reported ?? 0) === 0 && (unreported ?? 0) === 0) return "No cache-write usage reported";
  const pricedText = `${(priced ?? 0).toLocaleString()} of ${(reported ?? 0).toLocaleString()} reported writes priced`;
  return (unreported ?? 0) > 0 ? `${pricedText} · ${unreported?.toLocaleString()} requests without cache-write usage` : pricedText;
}

function coverageSummary(data: AIUsageReport): string {
  switch (data.coverage.status) {
    case "complete":
      return "Every model request reported provider usage, and every reported cache-write token was priced.";
    case "partial":
      return "Some model requests, cache writes, legacy records, or external runtimes are outside complete billing coverage.";
    case "unavailable":
      return "No provider request supplied complete usable accounting for this selection.";
  }
}

function AboutCoverage({ data }: { data: AIUsageReport }) {
  const [open, setOpen] = useState(false);
  const generatedID = useId();
  const contentID = `usage-coverage-details-${generatedID.replaceAll(":", "")}`;
  const states = data.coverage.states ?? [];

  return (
    <Box sx={{ borderTop: "1px solid", borderColor: "divider" }}>
      <ButtonBase
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        aria-controls={contentID}
        sx={{
          width: "100%",
          minHeight: 44,
          px: 1.5,
          py: 0.75,
          justifyContent: "flex-start",
          gap: 1,
          bgcolor: "surface.containerHigh",
          color: "text.primary",
          textAlign: "left",
          "&:hover": { bgcolor: "surface.containerHighest" },
          "&.Mui-focusVisible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: -2,
          },
        }}
      >
        <Typography component="span" sx={{ ...overviewTypography.secondaryBody, fontWeight: 700 }}>
          About coverage and estimates
        </Typography>
        <ChevronRight
          aria-hidden="true"
          sx={{
            ml: "auto",
            fontSize: 20,
            color: "text.secondary",
            transform: open ? "rotate(90deg)" : "rotate(0deg)",
            transition: (theme) => theme.transitions.create("transform", { duration: theme.transitions.duration.shortest }),
            "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          }}
        />
      </ButtonBase>
      <Collapse in={open} timeout="auto">
        <Box id={contentID} sx={{ px: 1.5, py: 1.5, display: "grid", gap: 1.25, borderTop: "1px solid", borderColor: "divider" }}>
          <Typography sx={overviewTypography.secondaryBody}>
            Provider-reported usage is the accounting source of truth. A present zero means the provider reported zero. An absent value remains unavailable and is never replaced with zero.
          </Typography>
          <Typography sx={overviewTypography.secondaryBody}>
            Recorded estimates use the price stored with each operation. Current-rate repricing applies prices configured now to historical provider-reported token totals. Current-rate repricing is not actual spend, and the two estimates do not form a meaningful delta.
          </Typography>
          {states.length > 0 && (
            <Box>
              <Typography color="text.secondary" sx={overviewTypography.tableHeading}>Coverage states</Typography>
              <Box component="ul" sx={{ my: 0.5, pl: 2.5, color: "text.secondary", ...overviewTypography.description }}>
                {states.map((state) => <li key={state}>{coverageStateLabel(state)}</li>)}
              </Box>
            </Box>
          )}
          <Typography sx={overviewTypography.secondaryBody}>
            Pricing configuration: {data.pricing_configured ? "configured" : "not configured"}.{" "}
            <Link
              href="https://github.com/willie-yao/prow-ai-dashboard/blob/main/docs/project-configuration.md"
              target="_blank"
              rel="noopener noreferrer"
            >
              Open ai.usage.pricing documentation
            </Link>
          </Typography>
        </Box>
      </Collapse>
    </Box>
  );
}

export function AIUsageCoverage({ data, partialDay }: { data: AIUsageReport; partialDay?: string }) {
  const coverage = data.coverage;
  const pricedReportedRequests = data.totals.priced_reported_requests ?? coverage.priced_reported_requests;
  const modelIdentity = data.selected_model || (data.models?.length === 1 ? data.models[0].model : data.models?.length ? "Mixed models" : "Unavailable");
  const modelCoverage = data.model_coverage ? data.model_coverage.replaceAll("_", " ") : "Unavailable";
  const pricingProvenance = data.pricing_rule || (data.pricing_configured ? "Configured pricing" : "Pricing not configured");
  const mixedWarningTitle = data.mixed_currency && data.mixed_pricing
    ? "Mixed recorded currencies and pricing"
    : data.mixed_currency
      ? "Mixed recorded currencies"
      : "Mixed recorded pricing";

  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand title="Coverage and pricing" metadata={`${coverageStatusLabel(coverage.status)} coverage`} />

      {(data.mixed_currency || data.mixed_pricing) && (
        <TraceNotice severity="warning" title={mixedWarningTitle}>
          {data.mixed_currency && data.mixed_pricing
            ? "Recorded estimates cannot be combined into one total and contain multiple stored pricing rules. Current-rate repricing remains a separate configured-rate view."
            : data.mixed_currency
              ? "Recorded estimates cannot be combined into one total. Current-rate repricing remains a separate configured-rate view."
              : "The selected range contains multiple stored pricing rules. Recorded estimates retain their per-operation pricing provenance."}
        </TraceNotice>
      )}
      {coverage.aggregate_overflow && (
        <TraceNotice severity="error" title="Aggregate overflow blocked">
          The report preserves an unavailable accounting state instead of publishing an overflowed total.
        </TraceNotice>
      )}

      <Box sx={{ minHeight: 60, display: "grid", gridTemplateColumns: { xs: "1fr", md: "190px minmax(0, 1fr)" }, gap: 2, alignItems: "start", px: 1.5, py: 1.25 }}>
        <UsageCoverageStatus status={coverage.status} />
        <Typography sx={overviewTypography.secondaryBody}>{coverageSummary(data)}</Typography>
      </Box>

      <Box sx={{ display: "grid", gridTemplateColumns: { xs: "repeat(2, minmax(0, 1fr))", md: "repeat(3, minmax(0, 1fr))" } }}>
        <CoverageItem label="Provider-reported requests">{formatCoverage(coverage.reported_requests, coverage.model_requests)}</CoverageItem>
        <CoverageItem label="Missing usage">{coverage.unreported_requests.toLocaleString()} requests</CoverageItem>
        <CoverageItem label="Priced-request coverage">{pricedRequestCoverageNote(pricedReportedRequests, coverage.reported_requests, data.pricing_coverage)}</CoverageItem>
        <CoverageItem label="Cache-write coverage">{cacheWriteCoverage(data)}</CoverageItem>
        <CoverageItem label="Model">{modelIdentity} · {modelCoverage}</CoverageItem>
        <CoverageItem label="Pricing provenance">{pricingProvenance}{data.currency ? ` · ${data.currency}` : ""}</CoverageItem>
        <CoverageItem label="External unmetered">{coverage.external_unmetered_operations.toLocaleString()} operations</CoverageItem>
        <CoverageItem label="Model gateway excluded">{(coverage.model_gateway_excluded_operations ?? 0).toLocaleString()} operations</CoverageItem>
        <CoverageItem label="Invalid provider usage">{(coverage.invalid_usage_requests ?? 0).toLocaleString()} requests</CoverageItem>
        <CoverageItem label="Pricing added later">{(coverage.pricing_added_after_requests ?? 0).toLocaleString()} requests</CoverageItem>
        <CoverageItem label="Legacy coverage">{coverage.legacy_coverage_unknown ? "Unknown for some records" : "Known"}</CoverageItem>
        <CoverageItem label="Current UTC day">{partialDay ? `${partialDay} is partial` : "No partial day in selection"}</CoverageItem>
      </Box>

      <AboutCoverage data={data} />
    </Box>
  );
}
