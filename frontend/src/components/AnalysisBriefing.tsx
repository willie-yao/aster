import Box from "@mui/material/Box";
import ButtonBase from "@mui/material/ButtonBase";
import Collapse from "@mui/material/Collapse";
import Divider from "@mui/material/Divider";
import Typography from "@mui/material/Typography";
import useMediaQuery from "@mui/material/useMediaQuery";
import { ChevronRight } from "@mui/icons-material";
import { useId, useState, type ReactNode } from "react";
import { useTheme } from "@mui/material/styles";
import { DetailSectionBand } from "./DetailSectionBand";
import { overviewTypography } from "../theme/overview";

function BriefingBody({
  summary,
  followUp,
  details,
}: {
  summary: ReactNode;
  followUp?: ReactNode;
  details?: ReactNode;
}) {
  return (
    <Box sx={{ px: { xs: 1.5, sm: 2 }, py: { xs: 1.75, sm: 2 } }}>
      <Box
        sx={{
          maxWidth: "68ch",
          color: "text.primary",
          fontSize: "16px",
          lineHeight: "25px",
          fontWeight: 550,
          overflowWrap: "anywhere",
        }}
      >
        {summary}
      </Box>
      {followUp && (
        <>
          <Divider sx={{ my: 2.25, borderColor: "divider" }} />
          <Box sx={{ maxWidth: "68ch", overflowWrap: "anywhere" }}>{followUp}</Box>
        </>
      )}
      {details && (
        <>
          <Divider sx={{ my: 2.25, borderColor: "divider" }} />
          <Box
            sx={{
              maxWidth: "68ch",
              display: "flex",
              flexDirection: "column",
              gap: 2.25,
              overflowWrap: "anywhere",
            }}
          >
            {details}
          </Box>
        </>
      )}
    </Box>
  );
}

export function AnalysisBriefing({
  id,
  title,
  icon,
  metadata,
  summary,
  mobileTitle,
  mobileSynopsis,
  mobileMetadata,
  mobileNotice,
  details,
  followUp,
  actions,
  collapseDetailsOnMobile = true,
}: {
  id?: string;
  title: string;
  icon?: ReactNode;
  metadata?: ReactNode;
  summary: ReactNode;
  mobileTitle?: string;
  mobileSynopsis?: ReactNode;
  mobileMetadata?: ReactNode;
  mobileNotice?: ReactNode;
  details?: ReactNode;
  followUp?: ReactNode;
  actions?: ReactNode;
  collapseDetailsOnMobile?: boolean;
}) {
  const theme = useTheme();
  const desktop = useMediaQuery(theme.breakpoints.up("md"));
  const [open, setOpen] = useState(false);
  const generatedID = useId();
  const contentID = `full-analysis-${generatedID.replaceAll(":", "")}`;

  if (desktop) {
    return (
      <Box
        id={id}
        component="section"
        sx={{
          minWidth: 0,
          maxWidth: "100%",
          bgcolor: "surface.container",
          borderBottom: "1px solid",
          borderColor: "divider",
        }}
      >
        <DetailSectionBand title={title} icon={icon} metadata={metadata} />
        <BriefingBody summary={summary} followUp={followUp} details={details} />
        {actions && (
          <Box sx={{ px: 2, pb: 1.5, borderTop: "1px solid", borderColor: "divider" }}>
            {actions}
          </Box>
        )}
      </Box>
    );
  }

  if (!collapseDetailsOnMobile) {
    return (
      <Box
        id={id}
        component="section"
        sx={{
          minWidth: 0,
          maxWidth: "100%",
          bgcolor: "surface.container",
          borderBottom: "1px solid",
          borderColor: "divider",
        }}
      >
        <DetailSectionBand
          title={mobileTitle ?? title}
          icon={icon}
          metadata={mobileMetadata ?? metadata}
        />
        {mobileNotice && <Box sx={{ px: 1.5, pt: 1.5 }}>{mobileNotice}</Box>}
        <BriefingBody summary={summary} followUp={followUp} details={details} />
        {actions && (
          <Box sx={{ px: 1.5, pb: 1.5, borderTop: "1px solid", borderColor: "divider" }}>
            {actions}
          </Box>
        )}
      </Box>
    );
  }

  return (
    <Box id={id} sx={{ display: "flex", flexDirection: "column", gap: 2 }}>
      <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand
          title={mobileTitle ?? title}
          icon={icon}
          metadata={mobileMetadata ?? metadata}
        />
        <Box sx={{ px: 1.5, py: 1.5, ...overviewTypography.primaryBody, overflowWrap: "anywhere" }}>
          {mobileSynopsis ?? summary}
        </Box>
        {mobileNotice && <Box sx={{ px: 1.5, pb: 1.5 }}>{mobileNotice}</Box>}
        {followUp && (
          <Box sx={{ px: 1.5, pb: 1.5, pt: 1.5, borderTop: "1px solid", borderColor: "divider" }}>
            {followUp}
          </Box>
        )}
      </Box>

      <Box component="section" sx={{ bgcolor: "surface.container", borderBottom: "1px solid", borderColor: "divider" }}>
        <DetailSectionBand title="Full analysis" metadata={open ? "Expanded" : "Collapsed"} />
        <ButtonBase
          type="button"
          onClick={() => setOpen((value) => !value)}
          aria-expanded={open}
          aria-controls={contentID}
          sx={{
            width: "100%",
            minHeight: 44,
            px: 1.5,
            py: 0.75,
            justifyContent: "flex-start",
            gap: 1,
            color: "text.primary",
            textAlign: "left",
            borderTop: "1px solid",
            borderColor: "divider",
            "&:hover": { bgcolor: "surface.containerHigh" },
            "&.Mui-focusVisible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          <Typography component="span" sx={{ ...overviewTypography.secondaryBody, fontWeight: 650 }}>
            {open
              ? "Hide root cause, remediation, evidence, sources, builds, and files"
              : "Show root cause, remediation, evidence, sources, builds, and files"}
          </Typography>
          <ChevronRight
            sx={{
              ml: "auto",
              flexShrink: 0,
              color: "text.secondary",
              transform: open ? "rotate(90deg)" : "rotate(0deg)",
              transition: (currentTheme) =>
                currentTheme.transitions.create("transform", {
                  duration: currentTheme.transitions.duration.shortest,
                }),
              "@media (prefers-reduced-motion: reduce)": { transition: "none" },
            }}
          />
        </ButtonBase>
        <Collapse in={open} timeout="auto">
          <Box id={contentID}>
            <BriefingBody summary={summary} details={details} />
          </Box>
        </Collapse>
      </Box>

      {actions && (
        <Box sx={{ bgcolor: "surface.container", borderBlock: "1px solid", borderColor: "divider", px: 1.5, py: 1 }}>
          {actions}
        </Box>
      )}
    </Box>
  );
}
