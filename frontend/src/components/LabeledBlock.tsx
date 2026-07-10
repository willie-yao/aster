import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { soft } from "../theme";

/**
 * LabeledBlock is a soft-bordered inset with a left accent bar and an
 * uppercase label. Used for AI Root Cause / Suggested Fix sections.
 */
export function LabeledBlock({
  label,
  accent,
  children,
}: {
  label: string;
  accent: "primary" | "warning" | "success" | "error";
  children: ReactNode;
}) {
  return (
    <Box
      sx={{
        borderRadius: "10px",
        borderLeft: "3px solid",
        borderColor: (t) => soft(t, accent, 0.5),
        bgcolor: (t) => (t.vars ?? t).palette.surface.containerLow,
        p: { xs: 1.5, sm: 2 },
      }}
    >
      <Typography
        variant="label"
        color="text.secondary"
        sx={{ fontWeight: 600, display: "block", mb: 0.75, textTransform: "uppercase" }}
      >
        {label}
      </Typography>
      {children}
    </Box>
  );
}
