import { useId, useState, type FormEvent } from "react";
import ChevronRight from "@mui/icons-material/ChevronRight";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import MenuItem from "@mui/material/MenuItem";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import useMediaQuery from "@mui/material/useMediaQuery";
import { useTheme } from "@mui/material/styles";
import {
  aiUsageFilterSummary,
  compactAIUsageFilterSummary,
  featureLabels,
  type AIUsageFilterValues,
} from "../lib/aiUsage";
import type { AIUsageFeature } from "../types/usage";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";

function UsageFilterFields({
  initial,
  onApply,
  onReset,
}: {
  initial: AIUsageFilterValues;
  onApply: (values: AIUsageFilterValues) => void;
  onReset: () => void;
}) {
  const [start, setStart] = useState(initial.start);
  const [end, setEnd] = useState(initial.end);
  const [feature, setFeature] = useState<AIUsageFeature | "">(initial.feature);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    onApply({ start, end, feature });
  }

  return (
    <Box component="form" onSubmit={submit} sx={{ bgcolor: "surface.container" }}>
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: {
            xs: "minmax(0, 1fr)",
            md: "repeat(3, minmax(0, 1fr)) auto",
          },
          alignItems: "end",
          gap: 1.5,
          p: 1.5,
        }}
      >
        <TextField
          type="date"
          size="small"
          label="Start date"
          value={start}
          onChange={(event) => setStart(event.target.value)}
          slotProps={{ inputLabel: { shrink: true } }}
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
        <TextField
          type="date"
          size="small"
          label="End date"
          value={end}
          onChange={(event) => setEnd(event.target.value)}
          slotProps={{ inputLabel: { shrink: true } }}
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
        <TextField
          select
          size="small"
          label="Feature"
          value={feature}
          onChange={(event) => setFeature(event.target.value as AIUsageFeature | "")}
          sx={{
            minWidth: 0,
            "& .MuiOutlinedInput-root": {
              minHeight: 44,
              borderRadius: "4px",
              bgcolor: "surface.containerLow",
            },
          }}
        >
          <MenuItem value="">All features</MenuItem>
          {Object.entries(featureLabels).map(([key, label]) => (
            <MenuItem key={key} value={key}>{label}</MenuItem>
          ))}
        </TextField>
        <Stack
          direction="row"
          sx={{
            minHeight: 44,
            alignItems: "center",
            gap: 1,
            flexWrap: "nowrap",
          }}
        >
          <Button type="submit" variant="contained" sx={{ minHeight: { xs: 44, md: 40 }, borderRadius: "4px" }}>
            Apply
          </Button>
          <Button type="button" onClick={onReset} sx={{ minHeight: { xs: 44, md: 40 }, borderRadius: "4px" }}>
            Reset
          </Button>
        </Stack>
      </Box>
    </Box>
  );
}

export function AIUsageFilters({
  values,
  custom,
  onApply,
  onReset,
}: {
  values: AIUsageFilterValues;
  custom: boolean;
  onApply: (values: AIUsageFilterValues) => void;
  onReset: () => void;
}) {
  const theme = useTheme();
  const desktop = useMediaQuery(theme.breakpoints.up("md"));
  const [open, setOpen] = useState(custom);
  const generatedID = useId();
  const contentID = `ai-usage-filters-${generatedID.replaceAll(":", "")}`;
  const summary = aiUsageFilterSummary(values);
  const compactSummary = compactAIUsageFilterSummary(values);

  if (desktop) {
    return (
      <Box component="section" aria-label="AI usage filters" sx={{ borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand title="Usage filters" metadata={summary} />
        <UsageFilterFields initial={values} onApply={onApply} onReset={onReset} />
      </Box>
    );
  }

  return (
    <Box component="section" aria-label="AI usage filters" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
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
        <Typography component="h2" sx={overviewTypography.majorHeading}>Usage filters</Typography>
        <Typography
          title={summary}
          color="textSecondary"
          sx={{
            maxWidth: 190,
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
            ...overviewTypography.data,
          }}
        >
          {compactSummary}
        </Typography>
        <ChevronRight
          aria-hidden="true"
          sx={{
            fontSize: 20,
            color: "text.secondary",
            transform: open ? "rotate(90deg)" : "rotate(0deg)",
            transition: (currentTheme) => currentTheme.transitions.create("transform", { duration: currentTheme.transitions.duration.shortest }),
            "@media (prefers-reduced-motion: reduce)": { transition: "none" },
          }}
        />
      </ButtonBase>
      <Collapse in={open} timeout="auto">
        <Box id={contentID}>
          <UsageFilterFields initial={values} onApply={onApply} onReset={onReset} />
        </Box>
      </Collapse>
    </Box>
  );
}
