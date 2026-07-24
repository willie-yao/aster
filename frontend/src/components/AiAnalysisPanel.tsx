import { useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { AutoAwesome, HistoryOutlined, PublishedWithChangesOutlined, Terminal, UndoOutlined } from "@mui/icons-material";
import { Link as RouterLink } from "react-router-dom";
import type { AIAnalysis } from "../types/dashboard";
import type { AnalysisChatReference } from "../types/analysisChat";
import { RichText } from "./RichText";
import { LabeledBlock } from "./LabeledBlock";
import { AnalysisChat } from "./AnalysisChat";
import { soft } from "../theme";
import { useAnalysisCorrections } from "../hooks/useData";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";
import { findAnalysisCorrection, revokeAnalysisCorrection } from "../lib/analysisCorrections";
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
  chatRef,
}: {
  analysis: AIAnalysis;
  fileCtx: FileToUrlContext;
  traceHref?: string;
  chatRef?: AnalysisChatReference;
}) {
  const { data: corrections, refetch } = useAnalysisCorrections();
  const { features } = useCapabilities();
  const auth = useAuth();
  const [showOriginal, setShowOriginal] = useState(false);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const [correctionError, setCorrectionError] = useState<string | null>(null);
  const correction = chatRef ? findAnalysisCorrection(corrections, chatRef) : undefined;
  const correctionStale = Boolean(
    correction?.status === "active" && correction.analysis.analysis_generated_at !== chatRef?.analysis_generated_at,
  );
  const correctionActive = correction?.status === "active" && !correctionStale;
  const displayedAnalysis = correctionActive
    ? { ...analysis, root_cause: correction.revision.root_cause, suggested_fix: correction.revision.suggested_fix }
    : analysis;
  const sevColor = severityAccent(displayedAnalysis.severity);

  async function revokeCorrection() {
    if (!correction || revokeBusy) return;
    setRevokeBusy(true);
    setCorrectionError(null);
    try {
      await revokeAnalysisCorrection(correction.id);
    } catch (error) {
      setCorrectionError(error instanceof Error ? error.message : "Could not revoke the correction.");
    } finally {
      refetch();
      setRevokeBusy(false);
    }
  }

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
            label={`Severity: ${displayedAnalysis.severity}`}
            sx={{
              fontWeight: 600,
              ...(sevColor !== "primary"
                ? { bgcolor: (t) => soft(t, sevColor, 0.2), color: `${sevColor}.main` }
                : { bgcolor: "action.selected", color: "text.secondary" }),
            }}
          />
          {correctionActive && <Chip size="small" color="success" variant="outlined" label="Maintainer corrected" />}
          {correctionStale && <Chip size="small" color="warning" variant="outlined" label="Correction stale" />}
          {correction?.status === "revoked" && <Chip size="small" variant="outlined" label="Correction revoked" />}
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

        {correctionError && <Alert severity="error" variant="outlined">{correctionError}</Alert>}
        {correction && (
          <Box sx={{ border: "1px solid", borderColor: correctionActive ? "success.main" : "divider", borderRadius: "10px", p: 1.25, bgcolor: (theme) => soft(theme, correctionActive ? "success" : "primary", 0.045) }}>
            <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap" }}>
              <PublishedWithChangesOutlined sx={{ fontSize: 18, color: correctionActive ? "success.main" : "text.secondary" }} />
              <Typography variant="body2" sx={{ fontWeight: 650 }}>
                {correctionActive
                  ? "Maintainer correction confirmed"
                  : correctionStale
                    ? "This correction targets an older generated analysis and is not applied."
                    : "This correction was revoked and the original analysis is restored."}
              </Typography>
              <Button size="small" startIcon={<HistoryOutlined />} onClick={() => setShowOriginal((value) => !value)} sx={{ ml: { sm: "auto" } }}>
                {showOriginal ? "Hide original" : "View original"}
              </Button>
              {correctionActive && features.analysis_corrections && auth.status === "authenticated" && (
                <Button size="small" color="inherit" startIcon={<UndoOutlined />} onClick={() => void revokeCorrection()} disabled={revokeBusy}>
                  {revokeBusy ? "Revoking" : "Revoke"}
                </Button>
              )}
            </Stack>
            {showOriginal && (
              <Box sx={{ mt: 1.25, pt: 1.25, borderTop: "1px solid", borderColor: "divider" }}>
                <Typography variant="caption" color="text.secondary">Original root cause</Typography>
                <Typography variant="body2" sx={{ mt: 0.25, whiteSpace: "pre-line" }}>{analysis.root_cause}</Typography>
                <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 1 }}>Original suggested fix</Typography>
                <Typography variant="body2" sx={{ mt: 0.25, whiteSpace: "pre-line" }}>{analysis.suggested_fix}</Typography>
              </Box>
            )}
          </Box>
        )}

        <LabeledBlock label="Root Cause" accent={sevColor}>
          <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
            <RichText text={displayedAnalysis.root_cause} steps fileCtx={fileCtx} />
          </Typography>
        </LabeledBlock>

        <LabeledBlock label="Suggested Fix" accent="primary">
          <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
            <RichText text={displayedAnalysis.suggested_fix} steps fileCtx={fileCtx} />
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

        {chatRef && (
          <AnalysisChat
            key={[
              chatRef.job_id,
              chatRef.build_id,
              chatRef.test_name,
              chatRef.suite_name,
              chatRef.class_name,
              chatRef.junit_file,
              chatRef.analysis_generated_at,
            ].join("\u0000")}
            analysisRef={chatRef}
            fileCtx={fileCtx}
            onCorrectionChanged={refetch}
          />
        )}
      </Stack>
    </Box>
  );
}
