import Box from "@mui/material/Box";
import Collapse from "@mui/material/Collapse";
import Typography from "@mui/material/Typography";
import ReportProblem from "@mui/icons-material/ReportProblem";
import Insights from "@mui/icons-material/Insights";
import ExpandMore from "@mui/icons-material/ExpandMore";
import CheckCircleOutlined from "@mui/icons-material/CheckCircleOutlined";
import type { ReactNode } from "react";
import { useMemo, useState } from "react";
import { Link as RouterLink } from "react-router-dom";
import { useFlakinessReport, useResolved } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { jobPath, testPath, testRunPath } from "../lib/routes";
import { shortJobName, shortTestName } from "../lib/utils";
import { Panel } from "./Panel";
import type { PatternAnalysis, TestFlakiness } from "../types/dashboard";

const MAX_ITEMS = 10;
const MAX_PATTERNS = 5;

interface ItemGroup {
  label: string;
  items: TestFlakiness[];
}

interface AttentionRowProps {
  to: string;
  marker: ReactNode;
  subject: string;
  summary: string;
  detail?: string;
  count?: string;
  signal?: string;
  muted?: boolean;
}

function AttentionRow({
  to,
  marker,
  subject,
  summary,
  detail,
  count,
  signal,
  muted = false,
}: AttentionRowProps) {
  return (
    <Box
      component={RouterLink}
      to={to}
      sx={{
        display: "grid",
        gridTemplateColumns: {
          xs: "18px minmax(0, 1fr)",
          md: "18px minmax(180px, 1fr) minmax(220px, 2fr) auto auto",
        },
        gridTemplateAreas: {
          xs: '"marker subject" ". summary" ". meta"',
          md: '"marker subject summary count signal"',
        },
        alignItems: "center",
        columnGap: 1.25,
        rowGap: { xs: 0.5, md: 0 },
        minHeight: 52,
        px: 1,
        py: 1,
        color: "inherit",
        textDecoration: "none",
        opacity: muted ? 0.72 : 1,
        borderTop: "1px solid",
        borderColor: "divider",
        transition: "background-color 140ms ease",
        "&:hover": { bgcolor: "surface.containerHigh" },
        "&:focus-visible": {
          outline: "2px solid",
          outlineColor: "primary.main",
          outlineOffset: -2,
        },
      }}
    >
      <Box sx={{ gridArea: "marker", display: "flex", alignItems: "center" }}>{marker}</Box>
      <Typography
        variant="data"
        title={subject}
        sx={{
          gridArea: "subject",
          minWidth: 0,
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          color: "text.primary",
        }}
      >
        {subject}
      </Typography>
      <Box sx={{ gridArea: "summary", minWidth: 0 }}>
        <Typography
          variant="body2"
          title={summary}
          sx={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
        >
          {summary}
        </Typography>
        {detail && (
          <Typography
            variant="caption"
            color="text.secondary"
            title={detail}
            sx={{ display: "block", mt: 0.25, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
          >
            {detail}
          </Typography>
        )}
      </Box>
      <Typography
        variant="data"
        color="text.secondary"
        sx={{ gridArea: "count", display: { xs: "none", md: "block" }, whiteSpace: "nowrap" }}
      >
        {count}
      </Typography>
      <Typography
        variant="caption"
        color="text.secondary"
        sx={{ gridArea: "signal", display: { xs: "none", md: "block" }, minWidth: 82, textAlign: "right", whiteSpace: "nowrap" }}
      >
        {signal}
      </Typography>
      {(count || signal) && (
        <Box
          sx={{
            gridArea: "meta",
            display: { xs: "flex", md: "none" },
            alignItems: "center",
            gap: 1.5,
            flexWrap: "wrap",
          }}
        >
          {count && <Typography variant="data" color="text.secondary">{count}</Typography>}
          {signal && <Typography variant="caption" color="text.secondary">{signal}</Typography>}
        </Box>
      )}
    </Box>
  );
}

export function NeedsAttention() {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const { data, loading, error } = useFlakinessReport();
  const { data: resolved } = useResolved();
  const [resolvedOpen, setResolvedOpen] = useState(false);

  const recurring = useMemo<PatternAnalysis[]>(
    () =>
      (data?.recurring_patterns ?? [])
        .filter((pattern) => pattern.job_id && !(pattern.id && resolved.resolved[pattern.id]))
        .slice(0, MAX_PATTERNS),
    [data, resolved],
  );

  const resolvedPatterns = useMemo<PatternAnalysis[]>(
    () =>
      (data?.recurring_patterns ?? []).filter(
        (pattern) => pattern.job_id && pattern.id && resolved.resolved[pattern.id],
      ),
    [data, resolved],
  );

  const groups = useMemo<ItemGroup[]>(() => {
    if (!data) return [];
    const broken = data.recently_broken ?? [];
    const persistent = data.persistent_failures ?? [];
    const flaky = data.most_flaky ?? [];
    const hasPrimary = broken.length > 0 || persistent.length > 0;

    if (hasPrimary) {
      let remaining = MAX_ITEMS;
      const result: ItemGroup[] = [];
      if (broken.length > 0) {
        const items = broken.slice(0, remaining);
        result.push({ label: "New regressions", items });
        remaining -= items.length;
      }
      if (persistent.length > 0 && remaining > 0) {
        result.push({ label: "Persistent failures", items: persistent.slice(0, remaining) });
      }
      return result;
    }
    return flaky.length > 0 ? [{ label: "Flaky tests", items: flaky.slice(0, MAX_ITEMS) }] : [];
  }, [data]);

  if (loading || error || !data) return null;

  if (recurring.length === 0 && groups.length === 0 && resolvedPatterns.length === 0) {
    return (
      <Panel
        sx={{
          borderRadius: "6px",
          p: { xs: 3, sm: 4 },
          height: "100%",
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          textAlign: "center",
          gap: 1,
        }}
      >
        <CheckCircleOutlined sx={{ fontSize: 28, color: "success.main" }} />
        <Typography variant="headline" component="h2">All clear</Typography>
        <Typography variant="body2" color="text.secondary">
          No tests currently need attention.
        </Typography>
      </Panel>
    );
  }

  const totalItems = recurring.length + groups.reduce((sum, group) => sum + group.items.length, 0);

  return (
    <Panel sx={{ borderRadius: "6px", overflow: "hidden", height: "100%" }}>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1, px: 2, py: 1.5 }}>
        <ReportProblem color="warning" sx={{ fontSize: 20 }} />
        <Typography variant="headline" component="h2">
          Needs attention
        </Typography>
        <Typography variant="data" color="text.secondary">
          {totalItems}
        </Typography>
      </Box>

      {recurring.length > 0 && (
        <Box component="section" aria-labelledby="recurring-patterns-heading" sx={{ px: 1 }}>
          <Typography
            id="recurring-patterns-heading"
            variant="label"
            component="h3"
            color="text.secondary"
            sx={{ px: 1, py: 1 }}
          >
            Recurring patterns
          </Typography>
          {recurring.map((pattern) => {
            const refreshStatus = data.pattern_refresh?.jobs?.[pattern.job_id ?? ""];
            const stale = refreshStatus && refreshStatus.state !== "current";
            return (
              <AttentionRow
                key={pattern.id ?? pattern.job_id ?? pattern.subject}
                to={jobPath(pattern.job_id ?? "")}
                marker={<Insights sx={{ fontSize: 17, color: "text.secondary" }} />}
                subject={shortJobName(pattern.subject, filePrefix)}
                summary={pattern.shared_root_cause || pattern.summary}
                count={`${pattern.builds_analyzed} builds`}
                signal={stale ? "Last known good" : `${pattern.confidence} confidence`}
              />
            );
          })}
        </Box>
      )}

      {groups.map((group) => (
        <Box key={group.label} component="section" aria-label={group.label} sx={{ px: 1, mt: 1 }}>
          <Typography variant="label" component="h3" color="text.secondary" sx={{ px: 1, py: 1 }}>
            {group.label}
          </Typography>
          {group.items.map((item) => {
            const failing = item.classification !== "flaky";
            return (
              <AttentionRow
                key={`${item.job_id}/${item.test_name}`}
                to={item.last_failure?.build_id
                  ? testRunPath(item.job_id, item.test_name, item.last_failure.build_id)
                  : testPath(item.job_id, item.test_name)}
                marker={
                  <Box
                    aria-hidden="true"
                    sx={{ width: 8, height: 8, borderRadius: "2px", bgcolor: failing ? "error.main" : "warning.main" }}
                  />
                }
                subject={shortJobName(item.job_name, filePrefix)}
                summary={shortTestName(item.test_name)}
                detail={item.last_failure?.failure_message}
                count={item.consecutive_failures > 0 ? `${item.consecutive_failures} consecutive` : undefined}
                signal={failing ? "Failing" : "Flaky"}
              />
            );
          })}
        </Box>
      ))}

      {resolvedPatterns.length > 0 && (
        <Box component="section" sx={{ px: 1, py: 1 }}>
          <Box
            component="button"
            type="button"
            onClick={() => setResolvedOpen((open) => !open)}
            aria-expanded={resolvedOpen}
            sx={{
              width: "100%",
              minHeight: 44,
              appearance: "none",
              border: 0,
              m: 0,
              px: 1,
              background: "transparent",
              cursor: "pointer",
              textAlign: "left",
              font: "inherit",
              color: "inherit",
              display: "flex",
              alignItems: "center",
              gap: 0.5,
              "&:hover": { bgcolor: "surface.containerHigh" },
              "&:focus-visible": {
                outline: "2px solid",
                outlineColor: "primary.main",
                outlineOffset: -2,
              },
            }}
          >
            <Typography variant="label" component="span" color="text.secondary">
              Resolved ({resolvedPatterns.length})
            </Typography>
            <ExpandMore
              sx={{
                fontSize: 18,
                color: "text.secondary",
                transition: "transform 140ms ease",
                transform: resolvedOpen ? "rotate(0deg)" : "rotate(-90deg)",
              }}
            />
          </Box>
          <Collapse in={resolvedOpen} timeout="auto" unmountOnExit>
            {resolvedPatterns.map((pattern) => {
              const entry = pattern.id ? resolved.resolved[pattern.id] : undefined;
              return (
                <AttentionRow
                  key={pattern.id ?? pattern.job_id ?? pattern.subject}
                  to={jobPath(pattern.job_id ?? "")}
                  marker={<CheckCircleOutlined sx={{ fontSize: 17, color: "success.main" }} />}
                  subject={shortJobName(pattern.subject, filePrefix)}
                  summary={pattern.shared_root_cause || pattern.summary}
                  detail={entry?.note}
                  signal="Resolved"
                  muted
                />
              );
            })}
          </Collapse>
        </Box>
      )}
    </Panel>
  );
}
