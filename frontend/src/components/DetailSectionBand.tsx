import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import type { SxProps, Theme } from "@mui/material/styles";
import { overviewLayout, overviewTypography, sectionBandSx } from "../theme/overview";

interface DetailSectionBandProps {
  title: string;
  icon?: ReactNode;
  metadata?: ReactNode;
  headingLevel?: "h2" | "h3";
  id?: string;
  /** Set to -1 to make the heading a programmatic focus target for in-page links. */
  headingTabIndex?: -1;
  sx?: SxProps<Theme>;
}

export function DetailSectionBand({
  title,
  icon,
  metadata,
  headingLevel = "h2",
  id,
  headingTabIndex,
  sx,
}: DetailSectionBandProps) {
  const headingStyle = headingLevel === "h2"
    ? overviewTypography.majorHeading
    : overviewTypography.categoryHeading;

  return (
    <Box
      sx={[
        {
          minHeight: overviewLayout.majorBandMinHeight,
          display: "grid",
          gridTemplateColumns: { xs: "minmax(0, 1fr)", sm: "auto minmax(0, 1fr)" },
          gridTemplateAreas: { xs: '"title" "metadata"', sm: '"title metadata"' },
          alignItems: "center",
          columnGap: 1.5,
          rowGap: 0.25,
          px: 1.5,
          py: 1,
          borderBlock: "1px solid",
          ...sectionBandSx(),
        },
        ...(sx ? (Array.isArray(sx) ? sx : [sx]) : []),
      ]}
    >
      <Box sx={{ gridArea: "title", minWidth: 0, display: "flex", alignItems: "center", gap: 0.75 }}>
        {icon}
        <Typography id={id} component={headingLevel} tabIndex={headingTabIndex} sx={headingStyle}>
          {title}
        </Typography>
      </Box>
      {metadata && (
        <Typography
          component="div"
          color="textSecondary"
          sx={{
            gridArea: "metadata",
            minWidth: 0,
            justifySelf: { xs: "start", sm: "end" },
            textAlign: { xs: "left", sm: "right" },
            ...overviewTypography.data,
          }}
        >
          {metadata}
        </Typography>
      )}
    </Box>
  );
}
