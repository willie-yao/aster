import type { ReactNode } from "react";
import Box from "@mui/material/Box";
import DialogTitle from "@mui/material/DialogTitle";
import IconButton from "@mui/material/IconButton";
import Typography from "@mui/material/Typography";
import { BuildOutlined, CloseOutlined } from "@mui/icons-material";
import { overviewTypography, sectionBandSx } from "../theme/overview";

interface FixPreparationWorkspaceProps {
  context: ReactNode;
  controls: ReactNode;
  notices: ReactNode;
  actions: ReactNode;
  closeDisabled: boolean;
  onClose: () => void;
}

const paneSx = {
  minWidth: 0,
  p: 3,
  overflowY: { md: "auto" },
  overflowWrap: "anywhere",
} as const;

export function FixPreparationWorkspace(props: FixPreparationWorkspaceProps) {
  return (
    <Box sx={{ flex: 1, minHeight: 0, display: "grid", gridTemplateRows: "auto minmax(0, 1fr) auto" }}>
      <Box sx={{ display: "flex", alignItems: "center", gap: 1.5, px: { xs: 1.5, md: 2.5 }, py: 1.25, borderBottom: "1px solid", ...sectionBandSx() }}>
        <BuildOutlined sx={{ color: "primary.main", fontSize: 22, flexShrink: 0 }} />
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <DialogTitle sx={{ p: 0, ...overviewTypography.majorHeading }}>Use this finding in a fix proposal</DialogTitle>
        </Box>
        <IconButton aria-label="Close" onClick={props.onClose} disabled={props.closeDisabled}>
          <CloseOutlined fontSize="small" />
        </IconButton>
      </Box>

      <Box sx={{ minHeight: 0, overflowY: "auto", display: "flex", flexDirection: "column" }}>
        <Box sx={{ px: 2.5, "&:not(:empty)": { pt: 2 } }}>{props.notices}</Box>
        {props.context && (
          <Box
            sx={{
              flex: 1,
              display: "grid",
              minHeight: { md: 0 },
              gridTemplateColumns: { xs: "minmax(0, 1fr)", md: "minmax(0, 1fr) minmax(320px, 45%)" },
            }}
          >
            <Box component="section" aria-label="Fix context" sx={paneSx}>{props.context}</Box>
            <Box
              component="section"
              aria-label="Prepare the fix"
              sx={{ ...paneSx, bgcolor: "surface.containerLow", borderLeft: { md: "1px solid" }, borderTop: { xs: "1px solid", md: 0 }, borderColor: "divider" }}
            >
              <Typography component="h3" sx={{ ...overviewTypography.categoryHeading, mb: 3 }}>Prepare the fix</Typography>
              {props.controls}
            </Box>
          </Box>
        )}
      </Box>

      <Box sx={{ bgcolor: "surface.containerHigh", borderTop: "1px solid", borderColor: "divider", minWidth: 0 }}>
        {props.actions}
      </Box>
    </Box>
  );
}
