import { useId, useState, type FormEvent } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import FilterAlt from "@mui/icons-material/FilterAlt";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import useMediaQuery from "@mui/material/useMediaQuery";
import { useTheme } from "@mui/material/styles";
import {
  analysisTraceActiveFilterCount,
  analysisTraceFilterKeys,
  analysisTraceFilterLabels,
} from "../lib/analysisTraces";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";

function activeFilterLabel(count: number): string {
  return `${count} active`;
}

function FilterFields({
  searchParams,
  onApply,
  onClear,
}: {
  searchParams: URLSearchParams;
  onApply: (params: URLSearchParams) => void;
  onClear: () => void;
}) {
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const next = new URLSearchParams();
    for (const key of analysisTraceFilterKeys) {
      const value = String(form.get(key) ?? "").trim();
      if (value) next.set(key, value);
    }
    onApply(next);
  }

  return (
    <Box component="form" onSubmit={submit} sx={{ bgcolor: "surface.container" }}>
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "1fr", md: "repeat(2, minmax(0, 1fr))", xl: "repeat(4, minmax(0, 1fr))" },
          gap: 1.5,
          p: 1.5,
        }}
      >
        {analysisTraceFilterKeys.map((key) => (
          <TextField
            key={key}
            size="small"
            name={key}
            label={analysisTraceFilterLabels[key]}
            defaultValue={searchParams.get(key) ?? ""}
            sx={{
              minWidth: 0,
              "& .MuiOutlinedInput-root": {
                minHeight: 44,
                borderRadius: "4px",
                bgcolor: "surface.containerLow",
                ...overviewTypography.data,
              },
            }}
          />
        ))}
        <Stack
          direction="row"
          sx={{
            gridColumn: "1 / -1",
            minWidth: 0,
            minHeight: 44,
            alignItems: "center",
            gap: 1,
            flexWrap: "wrap",
          }}
        >
          <Button
            type="submit"
            variant="contained"
            startIcon={<FilterAlt />}
            sx={{ minHeight: { xs: 44, md: 40 }, borderRadius: "4px" }}
          >
            Apply filters
          </Button>
          <Button
            type="button"
            onClick={onClear}
            sx={{ minHeight: { xs: 44, md: 40 }, borderRadius: "4px" }}
          >
            Clear all
          </Button>
          <Typography
            color="text.secondary"
            sx={{ ml: { md: "auto" }, width: { xs: "100%", md: "auto" }, ...overviewTypography.description }}
          >
            Download JSON uses the current URL filters.
          </Typography>
        </Stack>
      </Box>
    </Box>
  );
}

export function AnalysisTraceFilters({
  searchParams,
  onApply,
  onClear,
}: {
  searchParams: URLSearchParams;
  onApply: (params: URLSearchParams) => void;
  onClear: () => void;
}) {
  const theme = useTheme();
  const desktop = useMediaQuery(theme.breakpoints.up("md"));
  const activeCount = analysisTraceActiveFilterCount(searchParams);
  const [open, setOpen] = useState(activeCount > 0);
  const generatedID = useId();
  const contentID = `analysis-trace-filters-${generatedID.replaceAll(":", "")}`;

  if (desktop) {
    return (
      <Box component="section" aria-label="Trace filters" sx={{ borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand title="Filters" metadata={activeFilterLabel(activeCount)} />
        <FilterFields searchParams={searchParams} onApply={onApply} onClear={onClear} />
      </Box>
    );
  }

  return (
    <Box component="section" aria-label="Trace filters" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
      <ButtonBase
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        aria-controls={contentID}
        sx={{
          width: "100%",
          minHeight: 48,
          px: 1.5,
          py: 0.75,
          display: "grid",
          gridTemplateColumns: "minmax(0, 1fr) auto auto",
          alignItems: "center",
          gap: 1,
          bgcolor: "surface.containerHigh",
          borderBlock: "1px solid",
          borderColor: "divider",
          boxShadow: "inset 3px 0 0 var(--mui-palette-primary-main)",
          color: "text.primary",
          textAlign: "left",
          "&:hover": { bgcolor: "surface.containerHighest" },
          "&.Mui-focusVisible": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: -2,
          },
        }}
      >
        <Typography component="h2" sx={overviewTypography.majorHeading}>
          Filters
        </Typography>
        <Typography color="text.secondary" sx={overviewTypography.data}>
          {activeFilterLabel(activeCount)}
        </Typography>
        <ChevronRight
          aria-hidden="true"
          sx={{
            fontSize: 20,
            color: "text.secondary",
            transform: open ? "rotate(90deg)" : "rotate(0deg)",
            transition: (currentTheme) =>
              currentTheme.transitions.create("transform", { duration: currentTheme.transitions.duration.shortest }),
            "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          }}
        />
      </ButtonBase>
      <Collapse in={open} timeout="auto">
        <Box id={contentID}>
          <FilterFields searchParams={searchParams} onApply={onApply} onClear={onClear} />
        </Box>
      </Collapse>
    </Box>
  );
}
