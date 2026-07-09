import { styled } from "@mui/material/styles";
import Paper from "@mui/material/Paper";

// Translucent surface for cards, dropdowns and raised panels. Override
// radius and padding via `sx`.
// Cast back to `typeof Paper` so it keeps Paper's polymorphic `component` prop.
export const Panel = styled(Paper)(({ theme }) => ({
  backgroundColor: (theme.vars ?? theme).palette.surface.glass,
  backdropFilter: "blur(14px)",
  WebkitBackdropFilter: "blur(14px)",
  border: `1px solid ${(theme.vars ?? theme).palette.divider}`,
  backgroundImage: "none",
  boxShadow: "0 1px 2px rgba(0, 0, 0, 0.16)",
})) as typeof Paper;
