import Box from "@mui/material/Box";
import FormControl from "@mui/material/FormControl";
import MenuItem from "@mui/material/MenuItem";
import Select from "@mui/material/Select";
import Typography from "@mui/material/Typography";
import type { OverviewStatusFilter } from "../lib/dashboardOverview";
import { overviewTypography } from "../theme/overview";
import { StatusFilterTiles } from "./StatusFilterTiles";

const statusFilters: { label: string; value: OverviewStatusFilter }[] = [
  { label: "All", value: "ALL" },
  { label: "Healthy", value: "HEALTHY" },
  { label: "Flaky", value: "FLAKY" },
  { label: "Unreliable", value: "UNRELIABLE" },
  { label: "Latest run failed", value: "LATEST_FAILED" },
];

// Status tone for each filter; the label always names the state.
const statusTone: Record<OverviewStatusFilter, string> = {
  ALL: "var(--mui-palette-text-primary)",
  HEALTHY: "var(--mui-palette-success-main)",
  FLAKY: "var(--mui-palette-warning-main)",
  UNRELIABLE: "var(--mui-palette-error-main)",
  LATEST_FAILED: "var(--mui-palette-error-main)",
};

interface OverviewFiltersProps {
  statusFilter: OverviewStatusFilter;
  branchFilter: string;
  branches: string[];
  matchingJobs: number;
  onStatusChange: (value: OverviewStatusFilter) => void;
  onBranchChange: (value: string) => void;
}

export function OverviewFilters({
  statusFilter,
  branchFilter,
  branches,
  matchingJobs,
  onStatusChange,
  onBranchChange,
}: OverviewFiltersProps) {
  return (
    <Box
      component="section"
      aria-label="Overview filters"
      sx={{
        display: "grid",
        gridTemplateColumns: {
          xs: "1fr",
          sm: "minmax(180px, 220px) 1fr",
          md: "minmax(0, 1fr) minmax(180px, 220px) auto",
        },
        alignItems: "center",
        gap: { xs: 1.5, md: 2 },
        py: 1.5,
        borderBlock: "1px solid",
        borderColor: "divider",
      }}
    >
      <Box sx={{ gridColumn: { sm: "1 / -1", md: "auto" }, minWidth: 0 }}>
        <StatusFilterTiles
          label="Job status filter"
          value={statusFilter}
          onChange={onStatusChange}
          options={statusFilters.map((filter) => ({ ...filter, tone: statusTone[filter.value] }))}
        />
      </Box>

      <FormControl size="small" fullWidth>
        <Select
          value={branchFilter}
          onChange={(event) => onBranchChange(event.target.value)}
          inputProps={{ "aria-label": "Branch filter" }}
          // Only the inner select box takes the tap, and MUI's own rule wins
          // over a class selector here, so the 44px target is set inline. It
          // stays a block box because MUI ellipsises the selected value as a
          // text node, which a flex container would clip instead.
          SelectDisplayProps={{
            style: {
              minHeight: 44,
              boxSizing: "border-box",
              lineHeight: "44px",
              paddingTop: 0,
              paddingBottom: 0,
            },
          }}
          sx={{
            height: 44,
            borderRadius: "4px",
            ...overviewTypography.data,
            bgcolor: "surface.container",
          }}
        >
          <MenuItem value="ALL">All branches</MenuItem>
          {branches.map((branch) => (
            <MenuItem key={branch} value={branch} sx={overviewTypography.data}>
              {branch}
            </MenuItem>
          ))}
        </Select>
      </FormControl>

      <Typography
        variant="data"
        color="textSecondary"
        sx={{ justifySelf: { xs: "start", md: "end" }, ...overviewTypography.data }}
      >
        {matchingJobs} {matchingJobs === 1 ? "job" : "jobs"}
      </Typography>
    </Box>
  );
}
