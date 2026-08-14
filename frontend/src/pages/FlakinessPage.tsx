import { useId, useState } from "react";
import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Tab from "@mui/material/Tab";
import Tabs from "@mui/material/Tabs";
import Typography from "@mui/material/Typography";
import ChevronRight from "@mui/icons-material/ChevronRight";
import OpenInNewIcon from "@mui/icons-material/OpenInNew";
import { Link as RouterLink } from "react-router-dom";
import { DetailSectionBand } from "../components/DetailSectionBand";
import { ErrorState } from "../components/ErrorState";
import { FetchActivityIcon } from "../components/FetchStatus";
import { LoadingState } from "../components/LoadingState";
import { useFlakinessReport } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { useSharedFetchStatus } from "../hooks/useSharedFetchStatus";
import { fetchStatusPresentation } from "../lib/fetchStatus";
import { jobPath, testPath, testRunPath } from "../lib/routes";
import { formatPercent, shortJobName, shortTestName, timeAgo } from "../lib/utils";
import { overviewTypography } from "../theme/overview";
import type { BuildFailureSummary, TestFlakiness } from "../types/dashboard";

type FailureTab =
  | "most_flaky"
  | "persistent"
  | "recently_broken"
  | "build_failures";
type TestTab = Exclude<FailureTab, "build_failures">;

type TabDefinition = {
  label: string;
  value: FailureTab;
  tooltip: string;
};

const tabs: TabDefinition[] = [
  {
    label: "Flakiest tests",
    value: "most_flaky",
    tooltip:
      "Tests that alternate between passing and failing. Sorted by flip rate, the percentage of runs where the result changed from the previous run.",
  },
  {
    label: "Persistent failures",
    value: "persistent",
    tooltip:
      "Tests with at least 3 consecutive failures, sorted by current streak length.",
  },
  {
    label: "Recent failures",
    value: "recently_broken",
    tooltip:
      "Tests whose current failure streak began within 48 hours of this published snapshot.",
  },
  {
    label: "Build failures",
    value: "build_failures",
    tooltip:
      "Build-level failures that were not reported as JUnit test cases. These remain separate from test flakiness and pass-rate calculations.",
  },
];

function classificationLabel(
  classification: TestFlakiness["classification"],
): string {
  if (classification === "one-off") return "New failure streak";
  return classification.charAt(0).toUpperCase() + classification.slice(1);
}

function classificationColor(
  classification: TestFlakiness["classification"],
): "error.main" | "warning.main" | "text.secondary" {
  if (classification === "persistent") return "error.main";
  if (classification === "flaky") return "warning.main";
  return "text.secondary";
}

function metricValue(tab: TestTab, item: TestFlakiness): string {
  switch (tab) {
    case "most_flaky":
      return formatPercent(item.flip_rate);
    case "persistent":
      return `${item.consecutive_failures}×`;
    case "recently_broken":
      return item.first_failed_at ? timeAgo(item.first_failed_at) : "Not available";
  }
}

function metricLabel(tab: TestTab): string {
  switch (tab) {
    case "most_flaky":
      return "Flip rate";
    case "persistent":
      return "Consecutive";
    case "recently_broken":
      return "Since";
  }
}

function MetricCell({ label, value }: { label: string; value: string }) {
  return (
    <Box sx={{ minWidth: 0 }}>
      <Typography component="div" color="text.secondary" sx={overviewTypography.tableHeading}>
        {label}
      </Typography>
      <Typography
        component="div"
        color="text.primary"
        sx={{ mt: 0.25, ...overviewTypography.data, fontWeight: 700 }}
      >
        {value}
      </Typography>
    </Box>
  );
}

function ClassificationSignal({
  classification,
}: {
  classification: TestFlakiness["classification"];
}) {
  const color = classificationColor(classification);
  return (
    <Box
      sx={{
        minHeight: 44,
        display: "inline-flex",
        alignItems: "center",
        gap: 0.75,
        color,
        fontSize: "13px",
        fontWeight: 700,
      }}
    >
      <Box
        component="span"
        sx={{ width: 7, height: 7, borderRadius: "2px", bgcolor: "currentColor" }}
      />
      {classificationLabel(classification)}
    </Box>
  );
}

function TestRow({ item, tab }: { item: TestFlakiness; tab: TestTab }) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const [expanded, setExpanded] = useState(false);
  const detailsId = useId();
  const lastFailureMessage = item.last_failure?.failure_message;
  const analysisPath = item.last_failure?.build_id
    ? testRunPath(item.job_id, item.test_name, item.last_failure.build_id)
    : testPath(item.job_id, item.test_name);
  const testTitle = shortTestName(item.test_name);
  const jobTitle = shortJobName(item.job_name, filePrefix);

  return (
    <Box
      component="article"
      sx={{
        minWidth: 0,
        bgcolor: "surface.container",
        borderTopWidth: "1px",
        borderTopStyle: "solid",
        borderTopColor: "var(--mui-palette-divider)",
        boxShadow: `inset 3px 0 0 var(--mui-palette-${
          item.classification === "persistent"
            ? "error-main"
            : item.classification === "flaky"
              ? "warning-main"
              : "divider"
        })`,
      }}
    >
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: {
            xs: "minmax(0, 1fr) auto",
            md: "minmax(0, 1fr) 100px 100px 120px 92px",
          },
          gridTemplateAreas: {
            xs: '"primary details" "metrics metrics"',
            md: '"primary metric fail classification details"',
          },
          alignItems: "center",
          minWidth: 0,
        }}
      >
        <Box sx={{ gridArea: "primary", minWidth: 0, px: 1.5, py: 1.25 }}>
          <Link
            component={RouterLink}
            to={analysisPath}
            underline="none"
            title={item.test_name}
            aria-label={`Open analysis for ${testTitle} in ${jobTitle}`}
            sx={{
              minHeight: { xs: 44, md: 28 },
              display: "flex",
              alignItems: "center",
              color: "text.primary",
              fontSize: "14px",
              lineHeight: "20px",
              fontWeight: 680,
              overflowWrap: "anywhere",
              "&:hover": { color: "primary.main" },
              "&:focus-visible": {
                outline: "2px solid",
                outlineColor: "primary.main",
                outlineOffset: 2,
              },
            }}
          >
            {testTitle}
          </Link>
          <Link
            component={RouterLink}
            to={jobPath(item.job_id)}
            underline="none"
            title={item.job_name}
            sx={{
              display: "inline-block",
              mt: 0.25,
              maxWidth: "100%",
              color: "text.secondary",
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
              ...overviewTypography.data,
              fontSize: "13px",
              "&:hover": { color: "primary.main" },
            }}
          >
            {jobTitle}
          </Link>
          {lastFailureMessage && (
            <Typography
              color="text.secondary"
              title={lastFailureMessage}
              sx={{
                mt: 0.75,
                display: "-webkit-box",
                WebkitBoxOrient: "vertical",
                WebkitLineClamp: 2,
                overflow: "hidden",
                fontSize: "13.5px",
                lineHeight: "20px",
              }}
            >
              {lastFailureMessage}
            </Typography>
          )}
        </Box>

        <Box
          sx={{
            gridArea: "metrics",
            display: { xs: "grid", md: "contents" },
            gridTemplateColumns: { xs: "repeat(3, minmax(0, 1fr))" },
            gap: { xs: 1, md: 0 },
            px: { xs: 1.5, md: 0 },
            pb: { xs: 1.25, md: 0 },
          }}
        >
          <Box sx={{ gridArea: { md: "metric" }, px: { md: 1.25 } }}>
            <MetricCell label={metricLabel(tab)} value={metricValue(tab, item)} />
          </Box>
          <Box sx={{ gridArea: { md: "fail" }, px: { md: 1.25 } }}>
            <MetricCell label="Fail rate" value={formatPercent(item.fail_rate)} />
          </Box>
          <Box sx={{ gridArea: { md: "classification" }, px: { md: 1.25 } }}>
            <ClassificationSignal classification={item.classification} />
          </Box>
        </Box>

        <ButtonBase
          type="button"
          aria-controls={detailsId}
          aria-expanded={expanded}
          aria-label={`${expanded ? "Hide" : "Show"} details for ${item.test_name}`}
          onClick={() => setExpanded((value) => !value)}
          sx={{
            gridArea: "details",
            minWidth: { xs: 44, md: 92 },
            minHeight: 44,
            justifyContent: "center",
            gap: 0.25,
            px: 0.75,
            color: "text.secondary",
            fontSize: "13px",
            fontWeight: 650,
            "&:hover": { bgcolor: "surface.containerHighest", color: "text.primary" },
            "&.Mui-focusVisible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          <Box component="span" sx={{ display: { xs: "none", md: "inline" } }}>
            Details
          </Box>
          <ChevronRight
            sx={{
              fontSize: 18,
              transform: expanded ? "rotate(90deg)" : "rotate(0deg)",
              transition: (theme) =>
                theme.transitions.create("transform", {
                  duration: theme.transitions.duration.shortest,
                }),
              "@media (prefers-reduced-motion: reduce)": { transition: "none" },
            }}
          />
        </ButtonBase>
      </Box>

      <Collapse in={expanded} timeout="auto">
        <Box
          id={detailsId}
          role="region"
          aria-label={`Details for ${item.test_name}`}
          sx={{
            borderTopWidth: "1px",
            borderTopStyle: "solid",
            borderTopColor: "var(--mui-palette-divider)",
            px: { xs: 1.5, sm: 2 },
            py: 1.5,
          }}
        >
          <Stack spacing={2} sx={{ maxWidth: "74ch" }}>
            {lastFailureMessage && (
              <Box>
                <Typography component="h3" color="text.secondary" sx={overviewTypography.subsectionHeading}>
                  Last error
                </Typography>
                <Box
                  component="pre"
                  sx={{
                    mt: 0.75,
                    mb: 0,
                    p: 1.5,
                    borderRadius: "4px",
                    bgcolor: "surface.containerHigh",
                    color: "error.main",
                    fontFamily: overviewTypography.data.fontFamily,
                    fontSize: "13px",
                    lineHeight: "20px",
                    overflowX: "auto",
                    whiteSpace: "pre-wrap",
                  }}
                >
                  {lastFailureMessage}
                </Box>
              </Box>
            )}

            {item.error_patterns && item.error_patterns.length > 0 && (
              <Box>
                <Typography component="h3" color="text.secondary" sx={overviewTypography.subsectionHeading}>
                  Error patterns
                </Typography>
                <Box sx={{ mt: 0.75 }}>
                  {item.error_patterns.map((pattern, index) => (
                    <Box
                      key={`${pattern.error_hash}-${index}`}
                      sx={{
                        display: "grid",
                        gridTemplateColumns: "56px minmax(0, 1fr)",
                        gap: 1.5,
                        py: 0.75,
                        borderTopWidth: index === 0 ? 0 : "1px",
                        borderTopStyle: "solid",
                        borderTopColor: "var(--mui-palette-divider)",
                      }}
                    >
                      <Typography color="error.main" sx={{ ...overviewTypography.data, fontWeight: 700 }}>
                        {pattern.count}×
                      </Typography>
                      <Box sx={{ minWidth: 0 }}>
                        <Typography sx={{ fontSize: "14px", lineHeight: "20px" }}>
                          {pattern.normalized_message}
                        </Typography>
                        <Typography color="text.secondary" sx={overviewTypography.description}>
                          Example: {pattern.example_message}
                        </Typography>
                      </Box>
                    </Box>
                  ))}
                </Box>
              </Box>
            )}
          </Stack>
        </Box>
      </Collapse>
    </Box>
  );
}

function buildSeverityColor(
  severity?: string,
): "error.main" | "warning.main" | "info.main" | "text.secondary" {
  switch (severity?.toLowerCase()) {
    case "critical":
    case "high":
      return "error.main";
    case "medium":
      return "warning.main";
    case "low":
      return "info.main";
    default:
      return "text.secondary";
  }
}

function BuildSignal({ label, color }: { label: string; color: string }) {
  return (
    <Box
      sx={{
        minHeight: 36,
        display: "inline-flex",
        alignItems: "center",
        gap: 0.75,
        color,
        fontSize: "13px",
        fontWeight: 700,
      }}
    >
      <Box component="span" sx={{ width: 7, height: 7, borderRadius: "2px", bgcolor: "currentColor" }} />
      {label}
    </Box>
  );
}

function BuildFailureRow({ item }: { item: BuildFailureSummary }) {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const summaryId = useId();
  const severity = item.severity || (item.is_transient ? "Transient" : "Unavailable");
  const summary = item.summary || "No accepted build analysis is available for this run.";
  const jobTitle = shortJobName(item.job_name, filePrefix);
  const stateColor = item.analysis_state === "succeeded" ? "error.main" : "warning.main";

  return (
    <Box
      component="article"
      sx={{
        bgcolor: "surface.container",
        borderTopWidth: "1px",
        borderTopStyle: "solid",
        borderTopColor: "var(--mui-palette-divider)",
        boxShadow: `inset 3px 0 0 var(--mui-palette-${
          item.analysis_state === "succeeded" ? "error-main" : "warning-main"
        })`,
      }}
    >
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: {
            xs: "minmax(0, 1fr)",
            md: "minmax(0, 1fr) 120px 120px 190px",
          },
          alignItems: "center",
          minWidth: 0,
        }}
      >
        <Link
          component={RouterLink}
          to={item.job_detail_url}
          underline="none"
          aria-label={`Open details for ${item.job_name} build ${item.build_id}`}
          aria-describedby={summaryId}
          sx={{
            minWidth: 0,
            px: 1.5,
            py: 1.25,
            color: "text.primary",
            "&:hover": { bgcolor: "surface.containerHighest", textDecoration: "none" },
            "&:focus-visible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          <Typography sx={{ fontSize: "14px", lineHeight: "20px", fontWeight: 680 }}>
            {jobTitle}
          </Typography>
          <Typography color="text.secondary" sx={{ mt: 0.25, ...overviewTypography.data, fontSize: "13px" }}>
            Build {item.build_id}{item.started_at ? ` · ${timeAgo(item.started_at)}` : ""}
          </Typography>
          <Typography id={summaryId} color="text.secondary" sx={{ mt: 0.75, fontSize: "13.5px", lineHeight: "20px" }}>
            {summary}
          </Typography>
        </Link>

        <Box sx={{ px: { xs: 1.5, md: 1.25 } }}>
          <BuildSignal label={item.result || "Failed"} color={stateColor} />
        </Box>
        <Box sx={{ px: { xs: 1.5, md: 1.25 } }}>
          <BuildSignal label={severity} color={buildSeverityColor(item.severity)} />
        </Box>
        <Box
          sx={{
            minHeight: 44,
            display: "flex",
            alignItems: "center",
            gap: 1.5,
            flexWrap: "wrap",
            px: 1.5,
            py: { xs: 0.5, md: 0 },
          }}
        >
          {item.provenance === "cache" && (
            <Typography color="text.secondary" sx={overviewTypography.description}>
              Cached analysis
            </Typography>
          )}
          {item.is_transient && severity.toLowerCase() !== "transient" && (
            <Typography color="info.main" sx={overviewTypography.description}>
              Transient
            </Typography>
          )}
          {item.build_log_url && (
            <Link
              href={item.build_log_url}
              target="_blank"
              rel="noopener noreferrer"
              sx={{ minHeight: { xs: 44, md: 36 }, display: "inline-flex", alignItems: "center", gap: 0.5, fontSize: "13px", fontWeight: 650 }}
            >
              Build log <OpenInNewIcon sx={{ fontSize: 14 }} />
            </Link>
          )}
        </Box>
      </Box>
    </Box>
  );
}

function TabLabel({ label, count }: { label: string; count: number }) {
  return (
    <Box component="span" sx={{ display: "inline-flex", alignItems: "center", gap: 0.75 }}>
      <Box component="span">{label}</Box>
      <Box component="span" sx={{ ...overviewTypography.data, fontSize: "13px" }}>
        {count}
      </Box>
    </Box>
  );
}

function EmptyCategory({ title }: { title: string }) {
  return (
    <Box
      sx={{
        minHeight: 180,
        display: "grid",
        placeItems: "center",
        bgcolor: "surface.container",
        borderBottomWidth: "1px",
        borderBottomStyle: "solid",
        borderBottomColor: "var(--mui-palette-divider)",
        px: 2,
        py: 4,
        textAlign: "center",
      }}
    >
      <Typography color="text.secondary" sx={overviewTypography.categoryHeading}>
        {title}
      </Typography>
    </Box>
  );
}

export function FlakinessPage() {
  const { data, loading, error } = useFlakinessReport();
  const fetchStatus = useSharedFetchStatus();
  const [activeTab, setActiveTab] = useState<FailureTab>("most_flaky");

  if (loading) return <LoadingState />;
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
  const tabCounts: Record<FailureTab, number> = {
    most_flaky: data.most_flaky.length,
    persistent: data.persistent_failures.length,
    recently_broken: data.recently_broken.length,
    build_failures: buildFailures.length,
  };
  const activeDefinition = tabs.find((tab) => tab.value === activeTab) ?? tabs[0];
  const testItems = activeTab === "build_failures" ? [] : testListMap[activeTab];
  const activeCount = tabCounts[activeTab];
  const refreshStatus = fetchStatus?.state === "active" ? fetchStatus.status : undefined;
  const refreshPresentation = fetchStatus?.state === "active"
    ? fetchStatusPresentation(fetchStatus)
    : null;

  return (
    <Stack spacing={{ xs: 2.5, sm: 3.5 }}>
      <Box>
        <Typography
          component="h1"
          sx={{
            ...overviewTypography.pageHeadline,
            fontSize: { xs: "26px", sm: "30px" },
            lineHeight: { xs: "33px", sm: "38px" },
            fontWeight: 720,
          }}
        >
          Failure Trends
        </Typography>
        <Stack
          direction={{ xs: "column", sm: "row" }}
          spacing={{ xs: 1, sm: 2.5 }}
          sx={{ mt: 1, alignItems: { xs: "flex-start", sm: "stretch" } }}
        >
          <Box sx={{ display: "flex", alignItems: "flex-start", gap: 1 }}>
            <Box
              aria-hidden="true"
              sx={{ width: 8, height: 8, mt: 0.75, borderRadius: "2px", bgcolor: "success.main" }}
            />
            <Box>
              <Typography sx={{ ...overviewTypography.data, color: "text.primary" }}>
                Published results
              </Typography>
              <Typography color="text.secondary" sx={overviewTypography.description}>
                Updated {timeAgo(data.generated_at)}
              </Typography>
            </Box>
          </Box>

          {refreshStatus && (
            <Box
              sx={{
                display: "flex",
                alignItems: "flex-start",
                gap: 1,
                pl: { xs: 0, sm: 2.5 },
                borderInlineStartWidth: { xs: 0, sm: "1px" },
                borderInlineStartStyle: "solid",
                borderInlineStartColor: "var(--mui-palette-divider)",
              }}
            >
              <Box aria-hidden="true" sx={{ color: "info.main", display: "flex", mt: 0.25 }}>
                <FetchActivityIcon size={16} />
              </Box>
              <Box>
                <Typography sx={{ ...overviewTypography.data, color: "info.main" }}>
                  {refreshPresentation?.title ?? "Refresh in progress"}
                </Typography>
                <Typography color="text.secondary" sx={overviewTypography.description}>
                  {refreshPresentation?.detail ?? "Preparing the next published snapshot"}
                </Typography>
                <Typography color="text.secondary" sx={overviewTypography.description}>
                  {activeTab === "build_failures"
                    ? "Showing the last published build failures. A new snapshot is currently being prepared."
                    : "Published results remain available until the refresh completes."}
                </Typography>
              </Box>
            </Box>
          )}
        </Stack>
      </Box>

      <Box>
        <Tabs
          value={activeTab}
          onChange={(_, value: FailureTab) => setActiveTab(value)}
          variant="scrollable"
          scrollButtons="auto"
          aria-label="Failure trend views"
          sx={{
            minHeight: 44,
            bgcolor: "surface.container",
            borderBlockWidth: "1px",
            borderBlockStyle: "solid",
            borderBlockColor: "var(--mui-palette-divider)",
            "& .MuiTabs-flexContainer": { gap: 0 },
            "& .MuiTabs-indicator": { display: "none" },
            "& .MuiTab-root": {
              minHeight: 44,
              minWidth: 0,
              px: 1.5,
              py: 0.75,
              borderInlineEndWidth: "1px",
              borderInlineEndStyle: "solid",
              borderInlineEndColor: "var(--mui-palette-divider)",
              borderRadius: 0,
              color: "text.secondary",
              fontSize: "13px",
              fontWeight: 700,
              textTransform: "none",
              "&:hover": { bgcolor: "surface.containerHigh", color: "text.primary" },
              "&.Mui-selected": {
                bgcolor: "action.selected",
                color: "text.primary",
                boxShadow: "inset 0 -3px 0 var(--mui-palette-primary-main)",
              },
              "&.Mui-focusVisible": {
                outline: "2px solid",
                outlineColor: "primary.main",
                outlineOffset: -2,
              },
            },
          }}
        >
          {tabs.map((tab) => (
            <Tab
              key={tab.value}
              value={tab.value}
              aria-describedby={`failure-trends-${tab.value}-description`}
              label={<TabLabel label={tab.label} count={tabCounts[tab.value]} />}
              title={tab.tooltip}
            />
          ))}
        </Tabs>

        {tabs.map((tab) => (
          <Box
            component="span"
            id={`failure-trends-${tab.value}-description`}
            key={`${tab.value}-description`}
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
            {tab.tooltip}
          </Box>
        ))}
      </Box>

      <Box component="section" sx={{ minWidth: 0 }}>
        <DetailSectionBand
          title={activeDefinition.label}
          metadata={`${activeCount} ${activeCount === 1 ? "item" : "items"}`}
        />
        <Typography
          color="text.secondary"
          sx={{
            px: 1.5,
            py: 1.25,
            bgcolor: "surface.container",
            borderBottomWidth: "1px",
            borderBottomStyle: "solid",
            borderBottomColor: "var(--mui-palette-divider)",
            ...overviewTypography.secondaryBody,
          }}
        >
          {activeDefinition.tooltip}
        </Typography>

        {activeTab === "build_failures" ? (
          buildFailures.length === 0 ? (
            <EmptyCategory title="No build failures in this snapshot" />
          ) : (
            <Box>
              {buildFailures.map((item) => (
                <BuildFailureRow key={`${item.job_id}/${item.build_id}`} item={item} />
              ))}
            </Box>
          )
        ) : testItems.length === 0 ? (
          <EmptyCategory title="No tests match this category" />
        ) : (
          <Box>
            {testItems.map((item) => (
              <TestRow key={`${item.job_id}/${item.test_name}`} item={item} tab={activeTab} />
            ))}
          </Box>
        )}
      </Box>
    </Stack>
  );
}
