import Box from "@mui/material/Box";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";
import { soft } from "../theme";
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
        gridTemplateColumns: { xs: "1fr", sm: "auto 1fr" },
        alignItems: "center",
        gap: { xs: 1.5, sm: 2 },
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
          gridTemplateColumns: `repeat(${stateFilters.length}, minmax(0, 1fr))`,
          "& .MuiToggleButtonGroup-grouped": {
            minWidth: 0,
            minHeight: 44,
            px: 1.25,
            py: 0.5,
            border: "1px solid !important",
            borderColor: "divider !important",
            borderRadius: "0 !important",
            color: "text.secondary",
            textTransform: "none",
            ...overviewTypography.tableHeading,
            "&:first-of-type": { borderRadius: "4px 0 0 4px !important" },
            "&:last-of-type": { borderRadius: "0 4px 4px 0 !important" },
            "&:not(:first-of-type)": { ml: "-1px" },
            "&:hover": { bgcolor: "surface.containerHigh", color: "text.primary" },
            "&.Mui-selected": {
              position: "relative",
              zIndex: 1,
              color: "text.primary",
              borderColor: "primary.main !important",
              bgcolor: (theme) => soft(theme, "primary", 0.12),
              boxShadow: "inset 0 -3px 0 var(--mui-palette-primary-main)",
              "&:hover": { bgcolor: (theme) => soft(theme, "primary", 0.16) },
            },
            "&.Mui-focusVisible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: 1,
            },
          },
        }}
      >
        {stateFilters.map((filter) => (
          <ToggleButton key={filter.value} value={filter.value}>
            {filter.label}
            <Box component="span" sx={{ ml: 0.75, color: "text.secondary", fontWeight: 600 }}>
              {counts[filter.value] ?? 0}
            </Box>
          </ToggleButton>
        ))}
      </ToggleButtonGroup>

      <Typography
        variant="data"
        color="text.secondary"
        sx={{ justifySelf: { xs: "start", sm: "end" }, ...overviewTypography.data }}
      >
        {matching} {matching === 1 ? "pull request" : "pull requests"}
      </Typography>
    </Box>
  );
}
