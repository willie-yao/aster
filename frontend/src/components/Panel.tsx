import { styled } from "@mui/material/styles";
import Paper from "@mui/material/Paper";

// Bordered surface for panels and popovers. Callers own padding and radius.
export const Panel = styled(Paper)(({ theme }) => ({
  backgroundColor: (theme.vars ?? theme).palette.surface.container,
  border: `1px solid ${(theme.vars ?? theme).palette.divider}`,
  backgroundImage: "none",
  boxShadow: "none",
})) as typeof Paper;
