import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";
import { Insights } from "@mui/icons-material";
import type { PatternAnalysis } from "../types/dashboard";
import { confidenceColor } from "../lib/utils";
import { RichText } from "./RichText";
import { LabeledBlock } from "./LabeledBlock";
import { FailureActions } from "./FailureActions";
import { useResolved } from "../hooks/useData";
import { soft } from "../theme";

export function PatternBanner({
  pattern,
  jobID,
}: {
  pattern: PatternAnalysis;
  jobID?: string;
}) {
  const color = pattern.systemic ? "warning" : "success";
  const confColor = confidenceColor(pattern.confidence, color);

  const { data: resolved } = useResolved();
  const resolvedEntry = pattern.id ? resolved.resolved[pattern.id] : undefined;

  return (
    <Box
      component="section"
      className="ai-aurora"
      sx={{
        borderRadius: "12px",
        bgcolor: (t) => soft(t, color, 0.05),
        p: { xs: 2, sm: 2.5 },
      }}
    >
      <Stack spacing={2}>
        <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap" }}>
          <Insights sx={{ fontSize: 20, color: `${color}.main` }} />
          <Typography variant="label" sx={{ fontWeight: 600 }} color={`${color}.main`}>
            {pattern.systemic ? "Recurring failure pattern" : "No shared root cause"}
          </Typography>
          <Chip
            size="small"
            label={`${pattern.builds_analyzed} builds analyzed`}
            sx={{ bgcolor: "action.selected", color: "text.secondary", fontWeight: 600 }}
          />
          <Chip
            size="small"
            label={`Confidence: ${pattern.confidence}`}
            sx={{
              fontWeight: 600,
              ...(confColor
                ? { bgcolor: (t) => soft(t, confColor, 0.2), color: `${confColor}.main` }
                : { bgcolor: "action.selected", color: "text.secondary" }),
            }}
          />
          {resolvedEntry && (
            <Chip
              size="small"
              label="Resolved"
              sx={{
                fontWeight: 600,
                bgcolor: (t) => soft(t, "success", 0.2),
                color: "success.main",
              }}
            />
          )}
        </Stack>

        {resolvedEntry && (
          <Typography variant="caption" color="text.secondary">
            Marked resolved by {resolvedEntry.resolved_by}
            {resolvedEntry.note ? ` — ${resolvedEntry.note}` : ""}. Re-opens
            automatically if it recurs.
          </Typography>
        )}

        <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
          <RichText text={pattern.summary} steps />
        </Typography>

        {pattern.systemic && pattern.shared_root_cause && (
          <LabeledBlock label="Shared Root Cause" accent={color}>
            <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
              <RichText text={pattern.shared_root_cause} steps />
            </Typography>
          </LabeledBlock>
        )}

        {pattern.systemic && pattern.suggested_fix && (
          <LabeledBlock label="Suggested Fix" accent="primary">
            <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
              <RichText text={pattern.suggested_fix} steps />
            </Typography>
          </LabeledBlock>
        )}

        {pattern.systemic && pattern.shared_builds && pattern.shared_builds.length > 0 && (
          <Box>
            <Typography variant="label" color="text.secondary" sx={{ fontWeight: 600, display: "block", mb: 0.5 }}>
              Affected Builds
            </Typography>
            <Stack direction="row" spacing={1} sx={{ flexWrap: "wrap", rowGap: 1 }}>
              {pattern.shared_builds.map((b) => (
                <Link
                  key={b}
                  component={RouterLink}
                  to={jobID ? `/job/${encodeURIComponent(jobID)}?run=${b}` : "#"}
                  underline="none"
                  sx={{
                    fontFamily: "monospace",
                    fontSize: "0.8125rem",
                    px: 0.75,
                    py: 0.25,
                    borderRadius: "4px",
                    bgcolor: "action.selected",
                    color: "primary.main",
                    "&:hover": { bgcolor: (t) => soft(t, "primary", 0.15) },
                  }}
                >
                  {b}
                </Link>
              ))}
            </Stack>
          </Box>
        )}

        {pattern.systemic && pattern.id && <FailureActions failureID={pattern.id} />}
      </Stack>
    </Box>
  );
}
