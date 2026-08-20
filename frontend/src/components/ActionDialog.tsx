import Box from "@mui/material/Box";
import DialogTitle from "@mui/material/DialogTitle";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import {
  dialogGutter,
  overviewLayout,
  overviewTypography,
  sectionBandSx,
} from "../theme/overview";

/**
 * Dialog header wearing the same band as DetailSectionBand, so an overlay
 * reads as a section of the page it opened from rather than its own surface.
 */
export function DialogHeader({
  icon,
  accent = "primary",
  title,
  subtitle,
}: {
  icon: ReactNode;
  accent?: "primary" | "warning";
  title: string;
  subtitle?: string;
}) {
  return (
    <DialogTitle
      sx={{
        display: "flex",
        alignItems: "center",
        gap: 1,
        minHeight: overviewLayout.majorBandMinHeight,
        px: dialogGutter,
        py: 1.5,
        borderBottom: "1px solid",
        ...sectionBandSx(accent),
      }}
    >
      <Box sx={{ display: "flex", color: `${accent}.main`, flexShrink: 0 }}>{icon}</Box>
      <Box sx={{ minWidth: 0 }}>
        <Typography component="span" sx={{ display: "block", ...overviewTypography.majorHeading }}>
          {title}
        </Typography>
        {subtitle && (
          <Typography
            variant="caption"
            color="textSecondary"
            sx={{ display: "block", mt: 0.25 }}
          >
            {subtitle}
          </Typography>
        )}
      </Box>
    </DialogTitle>
  );
}
