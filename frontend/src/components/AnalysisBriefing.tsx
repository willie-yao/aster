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
  summaryAside,
  details,
}: {
  summary: ReactNode;
  // Rendered beside the summary on wide rows. The prose keeps its reading
  // measure, so the space left over carries something useful instead of
  // widening lines past what is comfortable to read.
  summaryAside?: ReactNode;
  details?: ReactNode;
}) {
  const proseSx = {
    maxWidth: "68ch",
    color: "text.primary",
    fontSize: "16px",
    lineHeight: "25px",
    fontWeight: 550,
    overflowWrap: "anywhere" as const,
  };

  return (
    <Box sx={{ px: { xs: 1.5, sm: 2 }, py: { xs: 1.75, sm: 2 } }}>
      {/* Narrative prose only. The details below carry cards, chips, and file
          paths, which have no reading measure to hold them to. */}
      {summaryAside ? (
        <Box
          sx={{
            display: "grid",
            gap: 2.5,
            alignItems: "start",
            gridTemplateColumns: "minmax(0, 1fr)",
            "@media (min-width: 1100px)": {
              // The aside is sized to its content and anchored to the right
              // edge, so the row's leftover width reads as a gutter between two
              // columns rather than an empty tail inside one.
              gridTemplateColumns: "minmax(0, 68ch) max-content",
              justifyContent: "space-between",
            },
          }}
        >
          <Box sx={proseSx}>{summary}</Box>
          {summaryAside}
        </Box>
      ) : (
        <Box sx={proseSx}>{summary}</Box>
      )}
      {details && (
        <>
          <Divider sx={{ my: 2.25, borderColor: "divider" }} />
          <Box
            sx={{
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
  summaryAside,
  mobileTitle,
  mobileSynopsis,
  mobileMetadata,
  mobileNotice,
  details,
  actions,
  collapseDetailsOnMobile = true,
}: {
  id?: string;
  title: string;
  icon?: ReactNode;
  metadata?: ReactNode;
  summary: ReactNode;
  summaryAside?: ReactNode;
  mobileTitle?: string;
  mobileSynopsis?: ReactNode;
  mobileMetadata?: ReactNode;
  mobileNotice?: ReactNode;
  details?: ReactNode;
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
        <BriefingBody
          summary={summary}
          summaryAside={summaryAside}
          details={details}
        />
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
        <BriefingBody summary={summary} details={details} />
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
