import Box from "@mui/material/Box";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";
import type {
  PullRequestStateCounts,
  PullRequestStateFilter,
} from "../lib/pullRequests";
import { overviewTypography } from "../theme/overview";

const stateFilters: { label: string; value: PullRequestStateFilter }[] = [
  { label: "All", value: "ALL" },
  { label: "Failing", value: "FAILING" },
  { label: "Pending", value: "PENDING" },
  { label: "Passing", value: "PASSING" },
  { label: "No runs", value: "UNKNOWN" },
];

// Status tone for each presubmit state; the label always names the state.
const stateTone: Record<PullRequestStateFilter, string> = {
  ALL: "var(--mui-palette-text-primary)",
  FAILING: "var(--mui-palette-error-main)",
  PENDING: "var(--mui-palette-warning-main)",
  PASSING: "var(--mui-palette-success-main)",
  UNKNOWN: "var(--mui-palette-text-secondary)",
};

interface PullRequestFiltersProps {
  stateFilter: PullRequestStateFilter;
  counts: PullRequestStateCounts;
  matching: number;
  onStateChange: (value: PullRequestStateFilter) => void;
}

export function PullRequestFilters({
  stateFilter,
  counts,
  matching,
  onStateChange,
}: PullRequestFiltersProps) {
  return (
    <Box
      component="section"
      aria-label="Pull request filters"
      sx={{
        display: "grid",
        gridTemplateColumns: { xs: "1fr", md: "minmax(0, 1fr) auto" },
        alignItems: "center",
        gap: { xs: 1.5, md: 2 },
        py: 1.5,
        borderBlock: "1px solid",
        borderColor: "divider",
      }}
    >
      <ToggleButtonGroup
        exclusive
        value={stateFilter}
        onChange={(_, value: PullRequestStateFilter | null) => {
          if (value) onStateChange(value);
        }}
        aria-label="Presubmit state"
        sx={{
          display: "grid",
          gap: "2px",
          gridTemplateColumns: {
            xs: "repeat(3, minmax(0, 1fr))",
            md: `repeat(${stateFilters.length}, minmax(0, 1fr))`,
          },
          "& .MuiToggleButtonGroup-grouped": {
            position: "relative",
            minWidth: 0,
            minHeight: 56,
            px: 1.25,
            pt: 1.25,
            pb: 0.75,
            display: "flex",
            flexDirection: "column",
            alignItems: "flex-start",
            justifyContent: "flex-end",
            gap: 0.25,
            ml: "0 !important",
            border: "0 !important",
            borderRadius: "0 !important",
            overflow: "hidden",
            bgcolor: "surface.container",
            color: "text.secondary",
            textTransform: "none",
            // Each state carries its status tone as a top edge and a faint wash.
            "&::before": { content: '""', position: "absolute", insetInline: 0, top: 0, height: 4, bgcolor: "var(--tone)" },
            "&::after": {
              content: '""',
              position: "absolute",
              inset: 0,
              bgcolor: "var(--tone)",
              opacity: 0.09,
              pointerEvents: "none",
            },
            "&:first-of-type": { borderRadius: "4px 0 0 4px !important" },
            "&:last-of-type": { borderRadius: "0 4px 4px 0 !important" },
            "&:hover": { bgcolor: "surface.containerHigh", color: "text.primary" },
            "&.Mui-selected": {
              zIndex: 1,
              color: "text.primary",
              bgcolor: "surface.containerHighest",
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: "-2px",
              "&:hover": { bgcolor: "surface.containerHighest" },
            },
            "&.Mui-focusVisible": { outline: "2px solid", outlineColor: "primary.main", outlineOffset: 1 },
          },
        }}
      >
        {stateFilters.map((filter) => (
          <ToggleButton key={filter.value} value={filter.value} sx={{ "--tone": stateTone[filter.value] }}>
            <Box component="span" sx={{ ...overviewTypography.data, fontSize: "18px", lineHeight: "22px", fontWeight: 700, color: "text.primary" }}>
              {counts[filter.value] ?? 0}
            </Box>
            <Box component="span" sx={{ ...overviewTypography.tableHeading, whiteSpace: "nowrap" }}>
              {filter.label}
            </Box>
          </ToggleButton>
        ))}
      </ToggleButtonGroup>

      <Typography
        variant="data"
        color="textSecondary"
        sx={{ justifySelf: { xs: "start", md: "end" }, ...overviewTypography.data }}
      >
        {matching} {matching === 1 ? "pull request" : "pull requests"}
      </Typography>
    </Box>
  );
}
