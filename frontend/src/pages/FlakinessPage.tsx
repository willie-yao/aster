import { useId, useState } from "react";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import LinearProgress from "@mui/material/LinearProgress";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Tab from "@mui/material/Tab";
import Tabs from "@mui/material/Tabs";
import Typography from "@mui/material/Typography";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import SentimentSatisfiedAltIcon from "@mui/icons-material/SentimentSatisfiedAlt";
import OpenInNewIcon from "@mui/icons-material/OpenInNew";
import { Link as RouterLink } from "react-router-dom";
import { ErrorState } from "../components/ErrorState";
import { FetchActivityIcon } from "../components/FetchStatus";
import { LoadingState } from "../components/LoadingState";
import { Panel } from "../components/Panel";
import { useFlakinessReport } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { useSharedFetchStatus } from "../hooks/useSharedFetchStatus";
import { analysisProgressBreakdown } from "../lib/fetchStatus";
import { jobPath, testPath, testRunPath } from "../lib/routes";
import { formatPercent, shortJobName, shortTestName, timeAgo } from "../lib/utils";
import { soft } from "../theme";
import type { BuildFailureSummary, TestFlakiness } from "../types/dashboard";

type Tab = "most_flaky" | "persistent" | "recently_broken" | "build_failures";
type TestTab = Exclude<Tab, "build_failures">;
type ClassificationColor = "error" | "warning" | "default";

const tabs: { label: string; value: Tab; tooltip: string }[] = [
  { label: "Most Flaky", value: "most_flaky", tooltip: "Tests that alternate between passing and failing. Sorted by flip rate, the percentage of runs where the result changed from the previous run." },
  { label: "Persistent Failures", value: "persistent", tooltip: "Tests that have failed 3 or more times in a row with the same error. These are consistently broken, not flaky." },
  { label: "Recently Broken", value: "recently_broken", tooltip: "Tests that started a new failure streak within the last 48 hours. These are likely new regressions." },
  { label: "Build Failures", value: "build_failures", tooltip: "Build-level failures that were not reported as JUnit test cases. These remain separate from test flakiness and pass-rate calculations." },
];

function classificationStyle(c: TestFlakiness["classification"]): ClassificationColor {
  switch (c) {
    case "persistent":
      return "error";
    case "flaky":
      return "warning";
    case "one-off":
      return "default";
  }
}

function classificationLabel(c: TestFlakiness["classification"]): string {
  return c.charAt(0).toUpperCase() + c.slice(1);
}

function metricValue(tab: TestTab, item: TestFlakiness): string {
  switch (tab) {
    case "most_flaky":
      return formatPercent(item.flip_rate);
    case "persistent":
      return `${item.consecutive_failures}×`;
    case "recently_broken":
      return item.first_failed_at ? timeAgo(item.first_failed_at) : "—";
  }
}

function metricLabel(tab: TestTab): string {
  switch (tab) {
    case "most_flaky":
      return "Flip Rate";
    case "persistent":
      return "Consecutive";
    case "recently_broken":
      return "Since";
  }
}

function TestRow({ item, tab }: { item: TestFlakiness; tab: TestTab }) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const [expanded, setExpanded] = useState(false);
  const detailsId = useId();
  const failPct = Math.round(item.fail_rate * 100);
  const progressValue = Math.min(100, Math.max(0, failPct));
  const classificationColor = classificationStyle(item.classification);
  const lastFailureMessage = item.last_failure?.failure_message;

  return (
    <Panel
      sx={{
        borderRadius: "12px",
        overflow: "hidden",
        position: "relative",
        "&::before": {
          content: '""',
          position: "absolute",
          insetBlock: 0,
          insetInlineStart: 0,
          width: 3,
          bgcolor: (theme) =>
            classificationColor === "default"
              ? (theme.vars ?? theme).palette.divider
              : soft(theme, classificationColor, 0.9),
        },
      }}
    >
      <Stack spacing={1} sx={{ px: { xs: 1.5, sm: 2 }, py: 1 }}>
        <Box
          sx={{
            alignItems: "center",
            display: "flex",
            flexWrap: { xs: "wrap", sm: "nowrap" },
            gap: { xs: 1.25, sm: 2 },
            minWidth: 0,
            width: "100%",
          }}
        >
          <Box
            sx={{
              flex: { xs: "1 1 100%", sm: "1 1 auto" },
              minWidth: 0,
              width: { xs: "100%", sm: "auto" },
            }}
          >
            <Link
              component={RouterLink}
              to={item.last_failure?.build_id
                ? testRunPath(item.job_id, item.test_name, item.last_failure.build_id)
                : testPath(item.job_id, item.test_name)}
              underline="none"
              title={item.test_name}
              sx={{
                color: "text.primary",
                display: "block",
                fontSize: "0.875rem",
                fontWeight: 600,
                overflow: "hidden",
                textOverflow: "ellipsis",
                transition: "color 150ms ease",
                whiteSpace: "nowrap",
                "&:hover": { color: "primary.main" },
              }}
            >
              {shortTestName(item.test_name)}
            </Link>
            <Link
              component={RouterLink}
              to={jobPath(item.job_id)}
              underline="none"
              title={item.job_name}
              variant="label"
              sx={{
                color: "text.secondary",
                display: "inline-block",
                maxWidth: "100%",
                overflow: "hidden",
                textOverflow: "ellipsis",
                transition: "color 150ms ease",
                whiteSpace: "nowrap",
                "&:hover": { color: "primary.main" },
              }}
            >
              {shortJobName(item.job_name, filePrefix)}
            </Link>
          </Box>

          <Box
            sx={{
              alignItems: "center",
              display: "flex",
              flex: { xs: "1 1 auto", sm: "0 0 auto" },
              flexWrap: "nowrap",
              gap: { xs: 1.25, sm: 2 },
              minWidth: 0,
            }}
          >
            <Box
              sx={{
                flexShrink: 0,
                textAlign: { xs: "left", sm: "right" },
                width: { xs: 72, sm: 80 },
              }}
            >
              <Typography variant="label" component="div" color="text.secondary">
                {metricLabel(tab)}
              </Typography>
              <Typography
                variant="data"
                component="div"
                color="text.primary"
                sx={{ fontSize: "0.9375rem", fontWeight: 700 }}
              >
                {metricValue(tab, item)}
              </Typography>
            </Box>

            <Box sx={{ flexShrink: 0, width: { xs: 100, sm: 96 } }}>
              <Typography variant="label" color="text.secondary" sx={{ mb: 0.5 }}>
                Fail {failPct}%
              </Typography>
              <LinearProgress
                variant="determinate"
                value={progressValue}
                color="error"
                sx={{
                  bgcolor: (theme) => soft(theme, "error", 0.14),
                  borderRadius: 999,
                  height: 8,
                  "& .MuiLinearProgress-bar": { borderRadius: 999 },
                }}
              />
            </Box>

            <Chip
              size="small"
              label={classificationLabel(item.classification)}
              color={classificationColor}
              sx={{
                bgcolor: (theme) =>
                  classificationColor === "default"
                    ? (theme.vars ?? theme).palette.action.selected
                    : soft(theme, classificationColor, 0.18),
                color:
                  classificationColor === "default"
                    ? "text.secondary"
                    : `${classificationColor}.main`,
                flexShrink: 0,
                fontSize: "0.75rem",
                fontWeight: 600,
                height: 24,
                px: 0.5,
              }}
            />
          </Box>

          <IconButton
            aria-controls={detailsId}
            aria-expanded={expanded}
            aria-label={`${expanded ? "Collapse" : "Expand"} details for ${item.test_name}`}
            onClick={() => setExpanded((value) => !value)}
            size="small"
            sx={{
              color: "text.secondary",
              flexShrink: 0,
              ml: { xs: "auto", sm: 0 },
            }}
          >
            <ExpandMoreIcon
              fontSize="small"
              sx={{
                transform: expanded ? "rotate(180deg)" : "rotate(0deg)",
                transition: "transform 150ms ease",
              }}
            />
          </IconButton>
        </Box>

        {lastFailureMessage && (
          <Typography
            variant="caption"
            color="text.secondary"
            title={lastFailureMessage}
            noWrap
            sx={{ display: "block" }}
          >
            {lastFailureMessage}
          </Typography>
        )}
      </Stack>

      <Collapse in={expanded} timeout="auto">
        <Box
          id={detailsId}
          role="region"
          aria-label={`Details for ${item.test_name}`}
          sx={{
            borderTop: "1px solid",
            borderColor: "divider",
            px: 2,
            py: 2,
          }}
        >
          <Stack spacing={2}>
            {lastFailureMessage && (
              <Box>
                <Typography
                  variant="label"
                  color="text.secondary"
                  sx={{ display: "block", mb: 0.75 }}
                >
                  Last Error
                </Typography>
                <Box
                  component="pre"
                  sx={{
                    bgcolor: (theme) => soft(theme, "error", 0.05),
                    borderRadius: "8px",
                    color: "error.main",
                    fontFamily: "monospace",
                    fontSize: "0.75rem",
                    lineHeight: 1.6,
                    m: 0,
                    overflowX: "auto",
                    p: 1.5,
                    whiteSpace: "pre-wrap",
                  }}
                >
                  {lastFailureMessage}
                </Box>
              </Box>
            )}

            {item.error_patterns && item.error_patterns.length > 0 && (
              <Box>
                <Typography
                  variant="label"
                  color="text.secondary"
                  sx={{ display: "block", mb: 1 }}
                >
                  Error Patterns
                </Typography>
                <Stack spacing={1}>
                  {item.error_patterns.map((pat, i) => (
                    <Box
                      key={`${pat.error_hash}-${i}`}
                      sx={{
                        alignItems: "flex-start",
                        display: "flex",
                        gap: 1.5,
                        minWidth: 0,
                      }}
                    >
                      <Chip
                        size="small"
                        label={`${pat.count}×`}
                        sx={{
                          bgcolor: (theme) => soft(theme, "error", 0.18),
                          color: "error.main",
                          flexShrink: 0,
                          fontSize: "0.75rem",
                          fontWeight: 600,
                          height: 22,
                        }}
                      />
                      <Box sx={{ flex: 1, minWidth: 0 }}>
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          title={pat.normalized_message}
                          noWrap
                          sx={{ display: "block" }}
                        >
                          {pat.normalized_message}
                        </Typography>
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          title={pat.example_message}
                          noWrap
                          sx={{ display: "block", opacity: 0.65 }}
                        >
                          e.g. {pat.example_message}
                        </Typography>
                      </Box>
                    </Box>
                  ))}
                </Stack>
              </Box>
            )}
          </Stack>
        </Box>
      </Collapse>
    </Panel>
  );
}

function buildSeverityColor(severity?: string): "error" | "warning" | "info" | "default" {
  switch (severity?.toLowerCase()) {
    case "critical":
    case "high":
      return "error";
    case "medium":
      return "warning";
    case "low":
      return "info";
    default:
      return "default";
  }
}

function BuildFailureRow({ item }: { item: BuildFailureSummary }) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const severity = item.severity || (item.is_transient ? "Transient" : "Unavailable");
  const summary = item.summary || "No accepted build analysis is available for this run.";

  return (
    <Panel
      component="article"
      sx={{
        borderRadius: "12px",
        overflow: "hidden",
        position: "relative",
        "&::before": {
          content: '""',
          position: "absolute",
          insetBlock: 0,
          insetInlineStart: 0,
          width: 3,
          bgcolor: (theme) => soft(theme, item.analysis_state === "succeeded" ? "error" : "warning", 0.9),
        },
      }}
    >
      <Stack spacing={1.25} sx={{ px: { xs: 1.5, sm: 2 }, py: 1.5 }}>
        <Stack direction={{ xs: "column", sm: "row" }} spacing={1.25} sx={{ alignItems: { sm: "center" }, minWidth: 0 }}>
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Link
              component={RouterLink}
              to={item.job_detail_url}
              underline="none"
              sx={{ color: "text.primary", display: "block", fontSize: "0.875rem", fontWeight: 700 }}
            >
              {shortJobName(item.job_name, filePrefix)}
            </Link>
            <Typography variant="caption" color="text.secondary">
              Build {item.build_id}{item.started_at ? ` · ${timeAgo(item.started_at)}` : ""}
            </Typography>
          </Box>
          <Stack direction="row" spacing={0.75} sx={{ alignItems: "center", flexWrap: "wrap" }}>
            {item.result && <Chip size="small" variant="outlined" label={item.result} />}
            <Chip size="small" color={buildSeverityColor(item.severity)} variant="outlined" label={severity} />
            {item.provenance === "cache" && <Chip size="small" label="Cached" />}
            {item.is_transient && severity.toLowerCase() !== "transient" && <Chip size="small" color="info" variant="outlined" label="Transient" />}
          </Stack>
        </Stack>

        <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: "anywhere" }}>
          {summary}
        </Typography>

        <Stack direction="row" spacing={1.5} sx={{ alignItems: "center", flexWrap: "wrap" }}>
          <Link
            component={RouterLink}
            to={item.job_detail_url}
            aria-label={`Open details for ${item.job_name} build ${item.build_id}`}
            underline="hover"
            sx={{ fontSize: "0.8125rem", fontWeight: 600 }}
          >
            Open details
          </Link>
          {item.build_log_url && (
            <Link href={item.build_log_url} target="_blank" rel="noopener noreferrer" sx={{ display: "inline-flex", alignItems: "center", gap: 0.5, fontSize: "0.8125rem" }}>
              Build log <OpenInNewIcon sx={{ fontSize: 15 }} />
            </Link>
          )}
        </Stack>
      </Stack>
    </Panel>
  );
}

export function FlakinessPage() {
  const { data, loading, error } = useFlakinessReport();
  const fetchStatus = useSharedFetchStatus();
  const [activeTab, setActiveTab] = useState<Tab>("most_flaky");

  if (loading) {
    return <LoadingState />;
  }

  if (error) {
    return <ErrorState message={error} onRetry={() => window.location.reload()} />;
  }

  if (!data) return null;

  const testListMap: Record<TestTab, TestFlakiness[]> = {
    most_flaky: data.most_flaky,
    persistent: data.persistent_failures,
    recently_broken: data.recently_broken,
  };
  const buildFailures = data.build_failures ?? [];
  const tabCounts: Record<Tab, number> = {
    most_flaky: data.most_flaky.length,
    persistent: data.persistent_failures.length,
    recently_broken: data.recently_broken.length,
    build_failures: buildFailures.length,
  };
  const testItems = activeTab === "build_failures" ? [] : testListMap[activeTab];
  const activeDescription = tabs.find((t) => t.value === activeTab)?.tooltip;
  const refreshStatus = fetchStatus?.state === "active" ? fetchStatus.status : undefined;
  const refreshProgress = refreshStatus ? analysisProgressBreakdown(refreshStatus) : null;

  return (
    <Stack spacing={4}>
      <Stack spacing={1.25}>
        <Typography variant="h4" component="h1">
          Failure Analysis
        </Typography>
        <Stack
          direction={{ xs: "column", sm: "row" }}
          spacing={{ xs: 1.25, sm: 2.5 }}
          sx={{ alignItems: { xs: "flex-start", sm: "stretch" } }}
        >
          <Stack direction="row" spacing={1} sx={{ alignItems: "flex-start" }}>
            <Box
              aria-hidden="true"
              sx={{
                width: 8,
                height: 8,
                mt: 0.5,
                borderRadius: "50%",
                bgcolor: "success.main",
                boxShadow: (theme) => `0 0 8px ${(theme.vars ?? theme).palette.success.main}`,
                flex: "0 0 auto",
              }}
            />
            <Box>
              <Typography variant="data" sx={{ display: "block", color: "text.primary", fontSize: "0.75rem" }}>
                Published results
              </Typography>
              <Typography variant="caption" color="text.secondary">
                Updated {timeAgo(data.generated_at)}
              </Typography>
            </Box>
          </Stack>

          {refreshStatus && (
            <Stack
              direction="row"
              spacing={1}
              sx={{
                alignItems: "flex-start",
                borderLeft: { xs: 0, sm: "1px solid" },
                borderColor: { sm: "divider" },
                pl: { xs: 0, sm: 2.5 },
              }}
            >
              <Box aria-hidden="true" sx={{ color: "info.main", display: "flex", mt: 0.25 }}>
                <FetchActivityIcon size={16} />
              </Box>
              <Box>
                <Typography variant="data" sx={{ display: "block", color: "info.main", fontSize: "0.75rem" }}>
                  Refresh in progress
                </Typography>
                <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
                  {refreshProgress && refreshProgress.total > 0
                    ? `${refreshProgress.ready} of ${refreshProgress.total} results ready`
                    : "Preparing the next published snapshot"}
                </Typography>
                <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
                  {activeTab === "build_failures"
                    ? "Showing the last published build failures. A new snapshot is currently being prepared."
                    : "Published results remain available until the refresh completes."}
                </Typography>
              </Box>
            </Stack>
          )}
        </Stack>
      </Stack>

      <Stack spacing={1.5}>
        <Tabs
          value={activeTab}
          onChange={(_, value: Tab) => setActiveTab(value)}
          variant="scrollable"
          scrollButtons="auto"
          aria-label="Failure analysis categories"
          sx={{
            minHeight: 34,
            "& .MuiTabs-flexContainer": { gap: 0.5 },
            "& .MuiTabs-indicator": { display: "none" },
            "& .MuiTab-root": {
              bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
              borderRadius: 999,
              color: "text.secondary",
              fontSize: "0.75rem",
              fontWeight: 600,
              minHeight: 34,
              minWidth: 0,
              px: 1.5,
              py: 0.5,
              textTransform: "none",
              transition: "background-color 150ms ease, color 150ms ease",
              "&:hover": {
                bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
              },
              "&.Mui-selected": {
                bgcolor: "primary.main",
                color: "primary.contrastText",
              },
            },
          }}
        >
          {tabs.map((t) => (
            <Tab
              key={t.value}
              value={t.value}
              aria-describedby={`failure-analysis-${t.value}-description`}
              label={`${t.label} ${tabCounts[t.value]}`}
              title={t.tooltip}
            />
          ))}
        </Tabs>

        {tabs.map((t) => (
          <Box
            component="span"
            id={`failure-analysis-${t.value}-description`}
            key={`${t.value}-description`}
            sx={{
              border: 0,
              clip: "rect(0 0 0 0)",
              height: "1px",
              m: "-1px",
              overflow: "hidden",
              p: 0,
              position: "absolute",
              whiteSpace: "nowrap",
              width: "1px",
            }}
          >
            {t.tooltip}
          </Box>
        ))}

        <Typography variant="body2" color="text.secondary">
          {activeDescription}
        </Typography>
      </Stack>

      {activeTab === "build_failures" ? (
        buildFailures.length === 0 ? (
          <Panel sx={{ borderRadius: "12px", px: 2, py: 8, textAlign: "center" }}>
            <Typography variant="h6" color="text.secondary">No build failures in this snapshot</Typography>
          </Panel>
        ) : (
          <Stack spacing={1.5}>
            {buildFailures.map((item) => <BuildFailureRow key={`${item.job_id}/${item.build_id}`} item={item} />)}
          </Stack>
        )
      ) : testItems.length === 0 ? (
        <Panel sx={{ borderRadius: "12px", px: 2, py: 8, textAlign: "center" }}>
          <Stack
            direction="row"
            spacing={1}
            sx={{ alignItems: "center", justifyContent: "center", color: "text.secondary" }}
          >
            <Typography variant="h6" color="inherit">
              No tests match this category
            </Typography>
            <SentimentSatisfiedAltIcon fontSize="small" />
          </Stack>
        </Panel>
      ) : (
        <Stack spacing={1.5}>
          {testItems.map((item) => (
            <TestRow
              key={`${item.job_id}/${item.test_name}`}
              item={item}
              tab={activeTab}
            />
          ))}
        </Stack>
      )}
    </Stack>
  );
}
