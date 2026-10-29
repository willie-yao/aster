import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type {
  PullRequestStateCounts,
  PullRequestStateFilter,
} from "../lib/pullRequests";
import { overviewTypography } from "../theme/overview";
import { StatusFilterTiles } from "./StatusFilterTiles";

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
      <StatusFilterTiles
        label="Presubmit state"
        value={stateFilter}
        onChange={onStateChange}
        options={stateFilters.map((filter) => ({
          ...filter,
          tone: stateTone[filter.value],
          count: counts[filter.value] ?? 0,
        }))}
      />

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
