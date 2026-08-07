import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { overviewTypography } from "../theme/overview";

export interface MetricStripItem {
  label: string;
  value: ReactNode;
  color?: "error.main" | "warning.main" | "success.main" | "text.primary";
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
        borderBlock: "1px solid",
        borderColor: "divider",
      }}
    >
      {items.map((item, index) => (
        <Box
          key={item.label}
          sx={{
            minWidth: 0,
            minHeight: { xs: 68, sm: 72 },
            px: { xs: 1.5, sm: 2 },
            py: 1.25,
            display: "flex",
            flexDirection: "column",
            justifyContent: "center",
            borderLeft: "1px solid",
            borderTop: {
              xs: index >= 2 ? "1px solid" : "none",
              md: "none",
            },
            borderColor: "divider",
            "&:nth-of-type(odd)": {
              borderLeft: { xs: "none", md: index === 0 ? "none" : "1px solid" },
            },
            ...(index === 0 ? { borderLeft: "none" } : {}),
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
        </Box>
      ))}
    </Box>
  );
}
