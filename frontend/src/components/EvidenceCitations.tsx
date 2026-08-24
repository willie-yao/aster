import ExpandMore from "@mui/icons-material/ExpandMore";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Collapse from "@mui/material/Collapse";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import useMediaQuery from "@mui/material/useMediaQuery";
import { useId, useState } from "react";
import {
  COLLAPSE_THRESHOLD,
  citationKey,
  citationSummary,
  formatCitationRange,
  usableCitations,
} from "../lib/evidenceCitations";
import type { EvidenceCitation } from "../types/dashboard";
import { overviewTypography } from "../theme/overview";
import { BriefingSection } from "./BriefingSection";

interface EvidenceCitationsProps {
  citations: EvidenceCitation[] | undefined;
  detailAppearance?: boolean;
}

// EvidenceCitations lists the artifact quotes backing an analysis. The engine
// only publishes a citation whose quote occurs at the claimed lines, so the
// quote shown here is the verification and needs no request to check.
export function EvidenceCitations({ citations, detailAppearance }: EvidenceCitationsProps) {
  const [expanded, setExpanded] = useState(false);
  const reduceMotion = useMediaQuery("(prefers-reduced-motion: reduce)");
  const listID = useId();
  const usable = usableCitations(citations);
  if (usable.length === 0) return null;

  const alwaysShown = usable.slice(0, COLLAPSE_THRESHOLD);
  const hidden = usable.slice(COLLAPSE_THRESHOLD);
  const body = (
    <Stack spacing={1.25}>
      <Typography variant="caption" color="textSecondary" sx={{ display: "block" }}>
        {citationSummary(usable)}
      </Typography>
      <Stack spacing={1.25}>
        {alwaysShown.map((citation) => (
          <Citation key={citationKey(citation)} citation={citation} />
        ))}
      </Stack>
      {hidden.length > 0 && (
        <>
          <Collapse in={expanded} timeout={reduceMotion ? 0 : "auto"}>
            <Stack id={listID} spacing={1.25} sx={{ pt: 1.25 }}>
              {hidden.map((citation) => (
                <Citation key={citationKey(citation)} citation={citation} />
              ))}
            </Stack>
          </Collapse>
          <Button
            size="small"
            aria-expanded={expanded}
            aria-controls={listID}
            onClick={() => setExpanded((open) => !open)}
            endIcon={(
              <ExpandMore
                sx={{
                  fontSize: 16,
                  transform: expanded ? "rotate(180deg)" : "rotate(0deg)",
                  transition: reduceMotion ? "none" : "transform 150ms ease",
                }}
              />
            )}
            sx={{ alignSelf: "flex-start", px: 0.5, textTransform: "none" }}
          >
            {expanded ? "Show less" : `Show ${hidden.length} more`}
          </Button>
        </>
      )}
    </Stack>
  );

  if (detailAppearance) return <BriefingSection label="Evidence">{body}</BriefingSection>;
  return (
    <Box>
      <Typography
        variant="label"
        color="textSecondary"
        sx={{ fontWeight: 600, display: "block", mb: 0.5 }}
      >
        Evidence
      </Typography>
      {body}
    </Box>
  );
}

function Citation({ citation }: { citation: EvidenceCitation }) {
  return (
    <Box sx={{ minWidth: 0 }}>
      <Box
        sx={{
          ...overviewTypography.data,
          color: "text.secondary",
          overflowWrap: "anywhere",
          mb: 0.5,
        }}
      >
        {citation.path}
        <Box component="span" sx={{ color: "text.disabled" }}>
          {" "}
          {formatCitationRange(citation)}
        </Box>
      </Box>
      <Box
        component="pre"
        sx={{
          m: 0,
          p: 1,
          borderLeft: "2px solid",
          borderColor: "divider",
          bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
          borderRadius: 0.5,
          fontFamily: "monospace",
          fontSize: "0.75rem",
          lineHeight: 1.6,
          // Quotes keep their original indentation, which is often what makes a
          // log line readable, so they wrap rather than scroll off.
          whiteSpace: "pre-wrap",
          overflowWrap: "anywhere",
        }}
      >
        {citation.quote}
      </Box>
    </Box>
  );
}
