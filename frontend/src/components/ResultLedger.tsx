import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";
import type { ResultLedgerFilter } from "../lib/jobDetail";

export type { ResultLedgerFilter } from "../lib/jobDetail";

const filterLabels: Record<ResultLedgerFilter, string> = {
  failed: "Failed",
  passed: "Passed",
  all: "All executed",
};

export function ResultLedger({
  filter,
  query,
  executedCount,
  skippedCount,
  shownCount,
  onFilterChange,
  onQueryChange,
  children,
}: {
  filter: ResultLedgerFilter;
  query: string;
  executedCount: number;
  skippedCount: number;
  shownCount: number;
  onFilterChange: (filter: ResultLedgerFilter) => void;
  onQueryChange: (query: string) => void;
  children: ReactNode;
}) {
  return (
    <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <DetailSectionBand
        title="Test results"
        metadata={`${executedCount.toLocaleString()} executed · ${skippedCount.toLocaleString()} skipped not shown`}
      />
      <Box
        sx={{
          minHeight: 58,
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr)", md: "minmax(240px, 1fr) auto auto" },
          alignItems: "center",
          gap: { xs: 1, md: 1.75 },
          px: 1.5,
          py: 1,
          borderBottom: "1px solid",
          borderColor: "divider",
        }}
      >
        <TextField
          value={query}
          onChange={(event) => onQueryChange(event.target.value)}
          placeholder="Filter tests by name or failure text"
          label="Filter test results"
          size="small"
          slotProps={{
            inputLabel: { shrink: true },
            input: {
              sx: {
                minHeight: 44,
                borderRadius: "4px",
                bgcolor: "background.default",
                fontSize: "14px",
              },
            },
          }}
        />
        <Box
          role="group"
          aria-label="Test result status"
          sx={{ display: "grid", gridTemplateColumns: "repeat(3, minmax(0, 1fr))" }}
        >
          {(Object.keys(filterLabels) as ResultLedgerFilter[]).map((value, index, values) => {
            const selected = value === filter;
            return (
              <ButtonBase
                key={value}
                type="button"
                aria-pressed={selected}
                onClick={() => onFilterChange(value)}
                sx={{
                  minWidth: { xs: 0, md: 96 },
                  minHeight: 44,
                  px: { xs: 0.75, sm: 1.5 },
                  border: "1px solid",
                  borderColor: selected ? "primary.main" : "divider",
                  borderRadius:
                    index === 0 ? "4px 0 0 4px" : index === values.length - 1 ? "0 4px 4px 0" : 0,
                  ml: index === 0 ? 0 : "-1px",
                  bgcolor: selected ? "action.selected" : "background.default",
                  color: selected ? "text.primary" : "text.secondary",
                  boxShadow: selected ? "inset 0 -3px 0 var(--mui-palette-primary-main)" : "none",
                  fontSize: { xs: "12px", sm: "13px" },
                  fontWeight: 700,
                  zIndex: selected ? 1 : 0,
                  "&:hover": { bgcolor: "surface.containerHigh" },
                  "&.Mui-focusVisible": {
                    zIndex: 2,
                    outline: "2px solid",
                    outlineColor: "primary.main",
                    outlineOffset: 2,
                  },
                }}
              >
                {filterLabels[value]}
              </ButtonBase>
            );
          })}
        </Box>
        <Typography
          component="div"
          color="text.secondary"
          sx={{ ...overviewTypography.data, whiteSpace: "nowrap" }}
        >
          {shownCount.toLocaleString()} shown
        </Typography>
      </Box>
      {children}
      <Typography
        component="p"
        color="text.secondary"
        sx={{ m: 0, px: 1.5, py: 1, borderTop: "1px solid", borderColor: "divider", ...overviewTypography.description }}
      >
        Skipped tests are summarized above and are not included in this ledger.
      </Typography>
    </Box>
  );
}
