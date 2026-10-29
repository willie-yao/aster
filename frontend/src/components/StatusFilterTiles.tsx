import Box from "@mui/material/Box";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import { overviewTypography } from "../theme/overview";

export interface StatusFilterTile<T extends string> {
  value: T;
  label: string;
  // CSS color for the tile's status edge and wash. The label names the state.
  tone: string;
  count?: number;
}

interface StatusFilterTilesProps<T extends string> {
  label: string;
  value: T;
  options: StatusFilterTile<T>[];
  onChange: (value: T) => void;
}

// StatusFilterTiles is an exclusive filter drawn as status tiles: a tone edge,
// an optional count, and the label that names the state.
export function StatusFilterTiles<T extends string>({
  label,
  value,
  options,
  onChange,
}: StatusFilterTilesProps<T>) {
  const counted = options.some((option) => option.count !== undefined);
  return (
    <ToggleButtonGroup
      exclusive
      value={value}
      onChange={(_, next: T | null) => {
        if (next) onChange(next);
      }}
      aria-label={label}
      sx={{
        display: "grid",
        gap: "2px",
        gridTemplateColumns: {
          xs: `repeat(${Math.min(options.length, 3)}, minmax(0, 1fr))`,
          md: `repeat(${options.length}, minmax(0, 1fr))`,
        },
        "& .MuiToggleButtonGroup-grouped": {
          position: "relative",
          minWidth: 0,
          minHeight: counted ? 56 : 44,
          px: 1.25,
          pt: counted ? 1.25 : 1,
          pb: counted ? 0.75 : 0.5,
          display: "flex",
          flexDirection: "column",
          alignItems: "flex-start",
          justifyContent: counted ? "flex-end" : "center",
          gap: 0.25,
          ml: "0 !important",
          border: "0 !important",
          borderRadius: "0 !important",
          overflow: "hidden",
          bgcolor: "surface.container",
          color: "text.secondary",
          textAlign: "left",
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
      {options.map((option) => (
        <ToggleButton key={option.value} value={option.value} sx={{ "--tone": option.tone }}>
          {option.count !== undefined && (
            <Box
              component="span"
              sx={{ ...overviewTypography.data, fontSize: "18px", lineHeight: "22px", fontWeight: 700, color: "text.primary" }}
            >
              {option.count}
            </Box>
          )}
          <Box component="span" sx={overviewTypography.tableHeading}>
            {option.label}
          </Box>
        </ToggleButton>
      ))}
    </ToggleButtonGroup>
  );
}
