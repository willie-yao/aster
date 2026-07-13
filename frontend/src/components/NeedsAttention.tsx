import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Collapse from "@mui/material/Collapse";
import Divider from "@mui/material/Divider";
import List from "@mui/material/List";
import ListItemButton from "@mui/material/ListItemButton";
import Typography from "@mui/material/Typography";
import ReportProblem from "@mui/icons-material/ReportProblem";
import Insights from "@mui/icons-material/Insights";
import ExpandMore from "@mui/icons-material/ExpandMore";
import CheckCircleOutlined from "@mui/icons-material/CheckCircleOutlined";
import { useMemo, useState } from "react";
import { Link as RouterLink } from "react-router-dom";
import { useFlakinessReport, useResolved } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { confidenceColor, shortJobName, shortTestName } from "../lib/utils";
import { soft } from "../theme";
import { Panel } from "./Panel";
import type { PatternAnalysis, TestFlakiness } from "../types/dashboard";

const MAX_ITEMS = 10;
// Recurring systemic patterns are highest-signal, so they lead the box. Cap
// them so a noisy fleet cannot crowd out test-level regressions below.
const MAX_PATTERNS = 5;

interface ItemGroup {
  label: string;
  items: TestFlakiness[];
}

export function NeedsAttention() {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const { data, loading, error } = useFlakinessReport();
  const { data: resolved } = useResolved();
  const [resolvedOpen, setResolvedOpen] = useState(false);

  // Backend already filters to systemic verdicts and ranks by confidence, then
  // builds. Drop entries missing a job link, hide admin-resolved patterns
  // (shown in a separate collapsed section), and cap for display.
  const recurring = useMemo<PatternAnalysis[]>(
    () =>
      (data?.recurring_patterns ?? [])
        .filter((p) => p.job_id && !(p.id && resolved.resolved[p.id]))
        .slice(0, MAX_PATTERNS),
    [data, resolved],
  );

  // Patterns a maintainer marked resolved: kept visible but tucked into a
  // collapsed section so the active list stays focused.
  const resolvedPatterns = useMemo<PatternAnalysis[]>(
    () => (data?.recurring_patterns ?? []).filter((p) => p.job_id && p.id && resolved.resolved[p.id]),
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
        const slice = broken.slice(0, remaining);
        result.push({ label: "New Regressions", items: slice });
        remaining -= slice.length;
      }

      if (persistent.length > 0 && remaining > 0) {
        result.push({ label: "Persistent Failures", items: persistent.slice(0, remaining) });
      }

      return result;
    }

    if (flaky.length > 0) {
      return [{ label: "Flaky Tests", items: flaky.slice(0, MAX_ITEMS) }];
    }

    return [];
  }, [data]);

  if (loading) return null;

  // Only claim "all clear" on a successful, empty load. A failed fetch leaves
  // data null; surface nothing rather than a false all-clear.
  if (error || !data) return null;

  if (recurring.length === 0 && groups.length === 0 && resolvedPatterns.length === 0) {
    return (
      <Panel
        elevation={0}
        sx={{
          borderRadius: "12px",
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
        <CheckCircleOutlined sx={{ fontSize: 32, color: "success.main" }} />
        <Typography variant="headline" component="h2" sx={{ fontSize: "1.05rem" }}>
          All clear
        </Typography>
        <Typography variant="body2" color="text.secondary">
          No tests currently need attention.
        </Typography>
      </Panel>
    );
  }

  const totalItems =
    recurring.length + groups.reduce((sum, g) => sum + g.items.length, 0);

  return (
    <Panel
      elevation={0}
      sx={{
        borderRadius: "12px",
        overflow: "hidden",
        height: "100%",
        display: "flex",
        flexDirection: "column",
      }}
    >
      <Box
        sx={{
          display: "flex",
          alignItems: "center",
          gap: 1,
          p: { xs: 2, sm: 2.5 },
          flexShrink: 0,
        }}
      >
        <ReportProblem color="warning" fontSize="small" />
        <Typography variant="headline" component="h2" sx={{ m: 0, fontSize: "1.25rem" }}>
          Needs Attention ({totalItems})
        </Typography>
      </Box>

      <List
        disablePadding
        sx={{
          flex: 1,
          minHeight: 0,
          overflowY: "auto",
          maxHeight: { xs: "60vh", md: "none" },
          px: { xs: 2, sm: 2.5 },
          pb: { xs: 2, sm: 2.5 },
        }}
      >
          {recurring.length > 0 && (
            <Box component="li" sx={{ listStyle: "none" }}>
              <Typography
                variant="label"
                component="p"
                color="text.secondary"
                sx={{ py: 1, textTransform: "uppercase" }}
              >
                Recurring Patterns
              </Typography>

              {recurring.map((pattern) => {
                const confColor = confidenceColor(pattern.confidence);
                return (
                  <ListItemButton
                    key={pattern.job_id ?? pattern.subject}
                    component={RouterLink}
                    to={`/job/${encodeURIComponent(pattern.job_id ?? "")}`}
                    sx={{
                      gap: 1.5,
                      px: 1,
                      py: 1,
                      borderRadius: "8px",
                      color: "inherit",
                      textDecoration: "none",
                      "&:hover": {
                        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                      },
                    }}
                  >
                    <Insights
                      sx={{ fontSize: 18, color: "warning.main", flexShrink: 0, mt: "2px" }}
                    />

                    <Box sx={{ minWidth: 0, flex: 1 }}>
                      <Typography variant="caption" color="text.secondary" noWrap>
                        {shortJobName(pattern.subject, filePrefix)}
                      </Typography>
                      <Typography variant="body2" color="text.primary" noWrap>
                        {pattern.shared_root_cause || pattern.summary}
                      </Typography>
                    </Box>

                    <Box
                      sx={{
                        display: "flex",
                        alignItems: "center",
                        gap: 1,
                        flexShrink: 0,
                      }}
                    >
                      <Chip
                        size="small"
                        label={`${pattern.builds_analyzed} builds`}
                        sx={{
                          height: 22,
                          bgcolor: "action.selected",
                          color: "text.secondary",
                          fontWeight: 600,
                          display: { xs: "none", sm: "flex" },
                        }}
                      />
                      <Chip
                        size="small"
                        label={pattern.confidence}
                        sx={{
                          height: 22,
                          fontWeight: 600,
                          ...(confColor
                            ? { bgcolor: (theme) => soft(theme, confColor, 0.15), color: `${confColor}.main` }
                            : { bgcolor: "action.selected", color: "text.secondary" }),
                        }}
                      />
                    </Box>
                  </ListItemButton>
                );
              })}
            </Box>
          )}

          {groups.map((group, gi) => (
            <Box key={group.label} component="li" sx={{ listStyle: "none" }}>
              {(gi > 0 || recurring.length > 0) && <Divider sx={{ my: 1 }} />}
              <Typography
                variant="label"
                component="p"
                color="text.secondary"
                sx={{ py: 1, textTransform: "uppercase" }}
              >
                {group.label}
              </Typography>

              {group.items.map((item) => (
                <ListItemButton
                  key={`${item.job_id}/${item.test_name}`}
                  component={RouterLink}
                  to={`/job/${encodeURIComponent(item.job_id)}/test/${encodeURIComponent(item.test_name)}${item.last_failure?.build_id ? `?run=${item.last_failure.build_id}` : ""}`}
                  sx={{
                    gap: 1.5,
                    px: 1,
                    py: 1,
                    borderRadius: "8px",
                    color: "inherit",
                    textDecoration: "none",
                    "&:hover": {
                      bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                    },
                  }}
                >
                  <Box
                    sx={{
                      width: 8,
                      height: 8,
                      borderRadius: "50%",
                      flexShrink: 0,
                      bgcolor:
                        item.classification === "flaky"
                          ? "warning.main"
                          : "error.main",
                    }}
                  />

                  <Box sx={{ minWidth: 0, flex: 1 }}>
                    <Typography variant="caption" color="text.secondary" noWrap>
                      {shortJobName(item.job_name, filePrefix)}
                    </Typography>
                    <Typography variant="body2" color="text.primary" noWrap>
                      {shortTestName(item.test_name)}
                    </Typography>
                  </Box>

                  <Box
                    sx={{
                      display: "flex",
                      alignItems: "center",
                      gap: 1,
                      flexShrink: 0,
                      minWidth: 0,
                    }}
                  >
                    {item.consecutive_failures > 0 && (
                      <Chip
                        size="small"
                        label={`${item.consecutive_failures}×`}
                        sx={{
                          height: 22,
                          bgcolor: (theme) => soft(theme, "error", 0.15),
                          color: "error.main",
                          fontWeight: 600,
                        }}
                      />
                    )}
                    {item.last_failure?.failure_message && (
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{
                          display: { xs: "none", sm: "block" },
                          maxWidth: 200,
                          overflow: "hidden",
                          textOverflow: "ellipsis",
                          whiteSpace: "nowrap",
                        }}
                      >
                        {item.last_failure.failure_message}
                      </Typography>
                    )}
                  </Box>
                </ListItemButton>
              ))}
            </Box>
          ))}

          {resolvedPatterns.length > 0 && (
            <Box component="li" sx={{ listStyle: "none" }}>
              {(recurring.length > 0 || groups.length > 0) && <Divider sx={{ my: 1 }} />}
              <Box
                component="button"
                type="button"
                onClick={() => setResolvedOpen((p) => !p)}
                aria-expanded={resolvedOpen}
                sx={{
                  width: "100%",
                  appearance: "none",
                  border: 0,
                  m: 0,
                  background: "transparent",
                  cursor: "pointer",
                  textAlign: "left",
                  font: "inherit",
                  color: "inherit",
                  display: "flex",
                  alignItems: "center",
                  gap: 0.5,
                  px: 0,
                  py: 1,
                }}
              >
                <Typography variant="label" component="span" color="text.secondary" sx={{ textTransform: "uppercase" }}>
                  Resolved ({resolvedPatterns.length})
                </Typography>
                <ExpandMore
                  sx={{
                    fontSize: 18,
                    color: "text.secondary",
                    transition: (t) => t.transitions.create("transform", { duration: t.transitions.duration.short }),
                    transform: resolvedOpen ? "rotate(0deg)" : "rotate(-90deg)",
                  }}
                />
              </Box>
              <Collapse in={resolvedOpen} timeout="auto" unmountOnExit>
                {resolvedPatterns.map((pattern) => {
                  const entry = pattern.id ? resolved.resolved[pattern.id] : undefined;
                  return (
                    <ListItemButton
                      key={pattern.id ?? pattern.job_id ?? pattern.subject}
                      component={RouterLink}
                      to={`/job/${encodeURIComponent(pattern.job_id ?? "")}`}
                      sx={{
                        gap: 1.5,
                        px: 1,
                        py: 1,
                        borderRadius: "8px",
                        color: "inherit",
                        textDecoration: "none",
                        opacity: 0.7,
                        "&:hover": {
                          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh,
                        },
                      }}
                    >
                      <CheckCircleOutlined sx={{ fontSize: 18, color: "success.main", flexShrink: 0, mt: "2px" }} />
                      <Box sx={{ minWidth: 0, flex: 1 }}>
                        <Typography variant="caption" color="text.secondary" noWrap>
                          {shortJobName(pattern.subject, filePrefix)}
                        </Typography>
                        <Typography variant="body2" color="text.primary" noWrap>
                          {pattern.shared_root_cause || pattern.summary}
                        </Typography>
                        {entry?.note && (
                          <Typography variant="caption" color="text.secondary" noWrap sx={{ display: "block" }}>
                            {entry.note}
                          </Typography>
                        )}
                      </Box>
                    </ListItemButton>
                  );
                })}
              </Collapse>
            </Box>
          )}
        </List>
    </Panel>
  );
}
