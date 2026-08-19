import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { overviewTypography } from "../theme/overview";

// Marks a briefing section so sections can target each other as siblings.
const briefingSectionClass = "briefing-section";

// BriefingSection is one labeled block inside an analysis briefing, shared by
// the per-test panel and the recurring-pattern banner so the treatment is
// defined once.
//
// Every section preceded by another carries a rule. A root cause routinely runs
// several hundred pixels, and the container gap alone did not read as a
// boundary against a block that tall. The rule keys on a preceding sibling
// section rather than on position, so neither an intervening non-section
// sibling (the status row, the correction panel) nor a future sibling that
// happens to be a section can change which block goes unruled.
export function BriefingSection({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <Box
      component="section"
      className={briefingSectionClass}
      sx={{
        minWidth: 0,
        [`.${briefingSectionClass} ~ &`]: {
          pt: 2.25,
          borderTop: "1px solid",
          borderColor: "divider",
        },
      }}
    >
      <Typography
        component="h3"
        color="textSecondary"
        sx={{ ...overviewTypography.subsectionHeading, fontSize: "14px", lineHeight: "20px" }}
      >
        {label}
      </Typography>
      <Box sx={{ mt: 0.75, fontSize: "16px", lineHeight: "25px" }}>{children}</Box>
    </Box>
  );
}
