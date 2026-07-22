import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { AutoAwesome, Terminal } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import type { AIAnalysis } from "../types/dashboard";
import { RichText } from "./RichText";
import { LabeledBlock } from "./LabeledBlock";
import { soft } from "../theme";
import { fileToUrl, fileSortKey, type FileToUrlContext } from "../lib/utils";

/** Accent color for a severity string. */
function severityAccent(sev: string): "error" | "warning" | "primary" {
  if (sev === "Critical" || sev === "High") return "error";
  if (sev === "Medium") return "warning";
  return "primary";
}

/**
 * AiAnalysisPanel renders a single test's deep AI analysis: root cause,
 * suggested fix, and cited files. Mirrors the job-level PatternBanner styling.
 */
export function AiAnalysisPanel({
  analysis,
  fileCtx,
  traceHref,
}: {
  analysis: AIAnalysis;
  fileCtx: FileToUrlContext;
  traceHref?: string;
}) {
  const sevColor = severityAccent(analysis.severity);
  return (
    <Box
      component="section"
      className="ai-aurora"
      sx={{
        borderRadius: "12px",
        bgcolor: (t) => soft(t, "primary", 0.05),
        p: { xs: 2, sm: 2.5 },
      }}
    >
      <Stack spacing={2}>
        <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap" }}>
          <AutoAwesome sx={{ fontSize: 20, color: "primary.main" }} />
          <Typography variant="label" sx={{ fontWeight: 600 }} color="primary.main">
            AI Analysis
          </Typography>
          <Chip
            size="small"
            label={`Severity: ${analysis.severity}`}
            sx={{
              fontWeight: 600,
              ...(sevColor !== "primary"
                ? { bgcolor: (t) => soft(t, sevColor, 0.2), color: `${sevColor}.main` }
                : { bgcolor: "action.selected", color: "text.secondary" }),
            }}
          />
          {traceHref && (
            <Button
              component={RouterLink}
              to={traceHref}
              size="small"
              startIcon={<Terminal sx={{ fontSize: 16 }} />}
              sx={{ ml: { sm: "auto" }, textTransform: "none" }}
            >
              Inspect trace
            </Button>
          )}
        </Stack>

        <LabeledBlock label="Root Cause" accent={sevColor}>
          <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
            <RichText text={analysis.root_cause} steps fileCtx={fileCtx} />
          </Typography>
        </LabeledBlock>

        <LabeledBlock label="Suggested Fix" accent="primary">
          <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
            <RichText text={analysis.suggested_fix} steps fileCtx={fileCtx} />
          </Typography>
        </LabeledBlock>

        {analysis.relevant_files && analysis.relevant_files.length > 0 && (
          <Box>
            <Typography
              variant="label"
              color="text.secondary"
              sx={{ fontWeight: 600, display: "block", mb: 0.5 }}
            >
              Files to Check
            </Typography>
            <Stack spacing={0.5}>
              {[...analysis.relevant_files]
                .sort((a, b) => fileSortKey(a, fileCtx) - fileSortKey(b, fileCtx))
                .map((f) => {
                  const url = fileToUrl(f, fileCtx);
                  return (
                    <Box
                      key={f}
                      sx={{ fontFamily: "monospace", fontSize: "0.75rem", overflowWrap: "anywhere" }}
                    >
                      {url ? (
                        <Link href={url} target="_blank" rel="noopener noreferrer" underline="hover">
                          {f}
                        </Link>
                      ) : (
                        <Box component="span" sx={{ color: "text.secondary" }}>
                          {f}
                        </Box>
                      )}
                    </Box>
                  );
                })}
            </Stack>
          </Box>
        )}
      </Stack>
    </Box>
  );
}
