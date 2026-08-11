import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { overviewTypography } from "../theme/overview";

export interface MetricStripItem {
  label: string;
  value: ReactNode;
  color?: "error.main" | "warning.main" | "success.main" | "text.primary";
  note?: ReactNode;
}

export function MetricStrip({
  items,
  label,
}: {
  items: MetricStripItem[];
  label: string;
}) {
  return (
    <Box
      component="section"
      aria-label={label}
      sx={{
        display: "grid",
        gridTemplateColumns: {
          xs: "repeat(2, minmax(0, 1fr))",
          md: `repeat(${items.length}, minmax(0, 1fr))`,
        },
        bgcolor: "surface.container",
        borderBlockWidth: "1px",
        borderBlockStyle: "solid",
        borderBlockColor: "var(--mui-palette-divider)",
      }}
    >
      {items.map((item, index) => (
        <Box
          key={item.label}
          sx={{
            minWidth: 0,
            minHeight: item.note ? { xs: 92, sm: 96 } : { xs: 68, sm: 72 },
            px: { xs: 1.5, sm: 2 },
            py: 1.25,
            display: "flex",
            flexDirection: "column",
            justifyContent: "center",
            borderInlineStartWidth: {
              xs: index % 2 === 1 ? "1px" : 0,
              md: index > 0 ? "1px" : 0,
            },
            borderInlineStartStyle: "solid",
            borderInlineStartColor: "var(--mui-palette-divider)",
            borderTopWidth: { xs: index >= 2 ? "1px" : 0, md: 0 },
            borderTopStyle: "solid",
            borderTopColor: "var(--mui-palette-divider)",
          }}
        >
          <Typography
            component="span"
            color="text.secondary"
            sx={{ ...overviewTypography.tableHeading, fontSize: "12px" }}
          >
            {item.label}
          </Typography>
          <Typography
            component="span"
            sx={{
              mt: 0.25,
              color: item.color ?? "text.primary",
              fontFamily: overviewTypography.data.fontFamily,
              fontSize: { xs: "19px", sm: "20px" },
              lineHeight: { xs: "25px", sm: "27px" },
              fontWeight: 700,
              fontFeatureSettings: overviewTypography.data.fontFeatureSettings,
              overflowWrap: "anywhere",
            }}
          >
            {item.value}
          </Typography>
          {item.note && (
            <Typography
              component="span"
              color="text.secondary"
              sx={{ mt: 0.25, ...overviewTypography.description, fontSize: "12.5px" }}
            >
              {item.note}
            </Typography>
          )}
        </Box>
      ))}
    </Box>
  );
}
