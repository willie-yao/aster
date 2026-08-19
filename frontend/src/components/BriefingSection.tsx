import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import type { ReactNode } from "react";
import { overviewTypography } from "../theme/overview";

// BriefingSection is one labeled block inside an analysis briefing, shared by
// the per-test panel and the recurring-pattern banner so the treatment is
// defined once.
//
// Every section after the first carries a rule. A root cause routinely runs
// several hundred pixels, and the gap alone did not read as a boundary against
// a block that tall. The rule is scoped with :not(:first-of-type) against the
// section element, so non-section siblings such as the status row or the chat
// panel never change which block counts as first.
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
      sx={{
        minWidth: 0,
        "&:not(:first-of-type)": {
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
