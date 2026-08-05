import Box from "@mui/material/Box";
import FormControl from "@mui/material/FormControl";
import MenuItem from "@mui/material/MenuItem";
import Select from "@mui/material/Select";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";
import { soft } from "../theme";
import type { OverviewStatusFilter } from "../lib/dashboardOverview";

const statusFilters: { label: string; value: OverviewStatusFilter }[] = [
  { label: "All", value: "ALL" },
  { label: "Passing", value: "PASSING" },
  { label: "Flaky", value: "FLAKY" },
  { label: "Failing", value: "FAILING" },
];

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
        gridTemplateColumns: { xs: "1fr", sm: "auto minmax(180px, 220px) 1fr" },
        alignItems: "center",
        gap: { xs: 1.5, sm: 2 },
        py: 1.5,
        borderBlock: "1px solid",
        borderColor: "divider",
      }}
    >
      <ToggleButtonGroup
        exclusive
        value={statusFilter}
        onChange={(_, value: OverviewStatusFilter | null) => {
          if (value) onStatusChange(value);
        }}
        aria-label="Status filter"
        sx={{
          display: "grid",
          gridTemplateColumns: "repeat(4, minmax(0, 1fr))",
          "& .MuiToggleButtonGroup-grouped": {
            minWidth: 0,
            minHeight: 36,
            px: 1.25,
            py: 0.5,
            border: "1px solid !important",
            borderColor: "divider !important",
            borderRadius: "4px !important",
            color: "text.secondary",
            textTransform: "none",
            "&:not(:first-of-type)": { ml: "-1px" },
            "&:hover": { bgcolor: "surface.containerHigh", color: "text.primary" },
            "&.Mui-selected": {
              position: "relative",
              zIndex: 1,
              color: "primary.main",
              borderColor: "primary.main !important",
              bgcolor: (theme) => soft(theme, "primary", 0.1),
              "&:hover": { bgcolor: (theme) => soft(theme, "primary", 0.14) },
            },
            "&.Mui-focusVisible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: 1,
            },
          },
        }}
      >
        {statusFilters.map((filter) => (
          <ToggleButton key={filter.value} value={filter.value}>
            {filter.label}
          </ToggleButton>
        ))}
      </ToggleButtonGroup>

      <FormControl size="small" fullWidth>
        <Select
          value={branchFilter}
          onChange={(event) => onBranchChange(event.target.value)}
          inputProps={{ "aria-label": "Branch filter" }}
          sx={{
            height: 36,
            borderRadius: "4px",
            fontFamily: "monospace",
            fontSize: "0.75rem",
            bgcolor: "surface.container",
          }}
        >
          <MenuItem value="ALL">All branches</MenuItem>
          {branches.map((branch) => (
            <MenuItem key={branch} value={branch} sx={{ fontFamily: "monospace", fontSize: "0.8125rem" }}>
              {branch}
            </MenuItem>
          ))}
        </Select>
      </FormControl>

      <Typography
        variant="data"
        color="text.secondary"
        sx={{ justifySelf: { xs: "start", sm: "end" } }}
      >
        {matchingJobs} {matchingJobs === 1 ? "job" : "jobs"}
      </Typography>
    </Box>
  );
}
