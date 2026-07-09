import { useState } from "react";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import LinearProgress from "@mui/material/LinearProgress";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Tab from "@mui/material/Tab";
import Tabs from "@mui/material/Tabs";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import SentimentSatisfiedAltIcon from "@mui/icons-material/SentimentSatisfiedAlt";
import { Link as RouterLink } from "react-router-dom";
import { ErrorState } from "../components/ErrorState";
import { LoadingState } from "../components/LoadingState";
import { Panel } from "../components/Panel";
import { useFlakinessReport } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { formatPercent, shortJobName, shortTestName, timeAgo } from "../lib/utils";
import { soft } from "../theme";
import type { TestFlakiness } from "../types/dashboard";

type Tab = "most_flaky" | "persistent" | "recently_broken";
type ClassificationColor = "error" | "warning" | "default";

const tabs: { label: string; value: Tab; tooltip: string }[] = [
  { label: "Most Flaky", value: "most_flaky", tooltip: "Tests that alternate between passing and failing. Sorted by flip rate — the percentage of runs where the result changed from the previous run." },
  { label: "Persistent Failures", value: "persistent", tooltip: "Tests that have failed 3 or more times in a row with the same error. These are consistently broken, not flaky." },
  { label: "Recently Broken", value: "recently_broken", tooltip: "Tests that started a new failure streak within the last 48 hours. These are likely new regressions." },
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

function metricValue(tab: Tab, item: TestFlakiness): string {
  switch (tab) {
    case "most_flaky":
      return formatPercent(item.flip_rate);
    case "persistent":
      return `${item.consecutive_failures}×`;
    case "recently_broken":
      return item.first_failed_at ? timeAgo(item.first_failed_at) : "—";
  }
}

function metricLabel(tab: Tab): string {
  switch (tab) {
    case "most_flaky":
      return "Flip Rate";
    case "persistent":
      return "Consecutive";
    case "recently_broken":
      return "Since";
  }
}

function TestRow({ item, tab }: { item: TestFlakiness; tab: Tab }) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const [expanded, setExpanded] = useState(false);
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
      <Accordion
        disableGutters
        elevation={0}
        expanded={expanded}
        onChange={(_, nextExpanded) => setExpanded(nextExpanded)}
        square={false}
        sx={{
          bgcolor: "transparent",
          backgroundImage: "none",
          boxShadow: "none",
          "&:before": { display: "none" },
          "&.Mui-expanded": { m: 0 },
        }}
      >
        <AccordionSummary
          expandIcon={<ExpandMoreIcon fontSize="small" />}
          sx={{
            minHeight: 0,
            px: { xs: 1.5, sm: 2 },
            py: 1,
            "&.Mui-expanded": { minHeight: 0 },
            "& .MuiAccordionSummary-content": {
              minWidth: 0,
              my: 0,
            },
            "& .MuiAccordionSummary-content.Mui-expanded": { my: 0 },
            "& .MuiAccordionSummary-expandIconWrapper": {
              color: "text.secondary",
            },
          }}
        >
          <Stack spacing={1} sx={{ minWidth: 0, width: "100%" }}>
            <Box
              sx={{
                alignItems: "center",
                display: "flex",
                flexWrap: { xs: "wrap", sm: "nowrap" },
                gap: { xs: 1.5, sm: 2 },
                minWidth: 0,
                width: "100%",
              }}
            >
              <Box sx={{ flex: 1, minWidth: 0 }}>
                <Link
                  component={RouterLink}
                  to={`/job/${encodeURIComponent(item.job_id)}/test/${encodeURIComponent(item.test_name)}${item.last_failure?.build_id ? `?run=${item.last_failure.build_id}` : ""}`}
                  onClick={(e) => e.stopPropagation()}
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
                  to={`/job/${encodeURIComponent(item.job_id)}`}
                  onClick={(e) => e.stopPropagation()}
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
                  flexShrink: 0,
                  textAlign: { xs: "left", sm: "right" },
                  width: { xs: 84, sm: 80 },
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

              <Box sx={{ flexShrink: 0, width: { xs: 120, sm: 96 } }}>
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
        </AccordionSummary>

        <AccordionDetails
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
        </AccordionDetails>
      </Accordion>
    </Panel>
  );
}

export function FlakinessPage() {
  const { data, loading, error } = useFlakinessReport();
  const [activeTab, setActiveTab] = useState<Tab>("most_flaky");

  if (loading) {
    return <LoadingState />;
  }

  if (error) {
    return <ErrorState message={error} onRetry={() => window.location.reload()} />;
  }

  if (!data) return null;

  const listMap: Record<Tab, TestFlakiness[]> = {
    most_flaky: data.most_flaky,
    persistent: data.persistent_failures,
    recently_broken: data.recently_broken,
  };

  const items = listMap[activeTab] ?? [];
  const activeDescription = tabs.find((t) => t.value === activeTab)?.tooltip;

  return (
    <Stack spacing={4}>
      <Stack spacing={0.75}>
        <Typography variant="h4" component="h1">
          Test Analysis
        </Typography>
        <Box sx={{ display: "flex", alignItems: "center", gap: 0.75 }}>
          <Box
            sx={{
              width: 7,
              height: 7,
              borderRadius: "50%",
              bgcolor: "success.main",
              boxShadow: (theme) => `0 0 8px ${(theme.vars ?? theme).palette.success.main}`,
            }}
          />
          <Typography variant="data" color="text.secondary" sx={{ fontSize: "0.75rem" }}>
            Updated {timeAgo(data.generated_at)}
          </Typography>
        </Box>
      </Stack>

      <Stack spacing={1.5}>
        <Tabs
          value={activeTab}
          onChange={(_, value: Tab) => setActiveTab(value)}
          variant="scrollable"
          scrollButtons="auto"
          aria-label="Test analysis categories"
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
              label={
                <Tooltip title={t.tooltip} enterDelay={400}>
                  <Box component="span">{t.label}</Box>
                </Tooltip>
              }
            />
          ))}
        </Tabs>

        <Typography variant="body2" color="text.secondary">
          {activeDescription}
        </Typography>
      </Stack>

      {items.length === 0 ? (
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
          {items.map((item) => (
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
