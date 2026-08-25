import { useEffect, useRef, useState } from "react";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import {
  AutoAwesome,
  HistoryOutlined,
  PublishedWithChangesOutlined,
  UndoOutlined,
} from "@mui/icons-material";
import type { AIAnalysis, PatternAnalysis } from "../types/dashboard";
import type { AnalysisChatReference } from "../types/analysisChat";
import { RichText } from "./RichText";
import { LabeledBlock } from "./LabeledBlock";
import { BriefingSection } from "./BriefingSection";
import { EvidenceCitations } from "./EvidenceCitations";
import { AnalysisChat } from "./AnalysisChat";
import { AnalysisTraceInspector, type AnalysisTraceReference } from "./AnalysisTraceInspector";
import { UpstreamCauseNotice } from "./UpstreamCauseNotice";
import { externalCause } from "../lib/patternFixGuidance";
import { soft, softChipSx } from "../theme";
import { useAnalysisCorrections } from "../hooks/useData";
import { useCapabilities } from "../hooks/useCapabilities";
import { useAuth } from "../hooks/useAuth";
import {
  findAnalysisCorrection,
  revokeAnalysisCorrection,
} from "../lib/analysisCorrections";
import {
  fileToUrl,
  fileSortKey,
  type FileToUrlContext,
} from "../lib/utils";
import { overviewTypography } from "../theme/overview";

function severityAccent(severity: string): "error" | "warning" | "primary" {
  if (severity === "Critical" || severity === "High") return "error";
  if (severity === "Medium") return "warning";
  return "primary";
}

export function AiAnalysisPanel({
  analysis,
  fileCtx,
  buildWebURL,
  traceRef,
  chatRef,
  fixPatterns = [],
  appearance = "default",
  severityInHeader = false,
}: {
  analysis: AIAnalysis;
  fileCtx: FileToUrlContext;
  // Browsable artifact root for the build this analysis came from, used to link
  // cited artifacts.
  buildWebURL?: string;
  traceRef?: AnalysisTraceReference;
  chatRef?: AnalysisChatReference;
  fixPatterns?: PatternAnalysis[];
  appearance?: "default" | "detail";
  // Set when the surrounding header already states the severity, so the panel
  // does not repeat it a few lines below. Callers without such a header leave
  // this false and keep the chip as their only severity signal.
  severityInHeader?: boolean;
}) {
  const { data: corrections, error: correctionsLoadError, refetch } =
    useAnalysisCorrections();
  const { features } = useCapabilities();
  const auth = useAuth();
  const detailAppearance = appearance === "detail";
  const [showOriginal, setShowOriginal] = useState(false);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const [correctionError, setCorrectionError] = useState<string | null>(null);
  const revokeControllerRef = useRef<AbortController | null>(null);
  const analysisIdentity = [
    chatRef?.job_id,
    chatRef?.build_id,
    chatRef?.test_name,
    chatRef?.source,
    chatRef?.suite_name,
    chatRef?.class_name,
    chatRef?.junit_file,
    chatRef?.analysis_generated_at,
  ].join("\u0000");
  const identityRef = useRef(analysisIdentity);
  identityRef.current = analysisIdentity;

  useEffect(() => {
    revokeControllerRef.current?.abort();
    setShowOriginal(false);
    setRevokeBusy(false);
    setCorrectionError(null);
  }, [analysisIdentity]);

  const correction = chatRef
    ? findAnalysisCorrection(corrections, chatRef)
    : undefined;
  const correctionStale = Boolean(
    correction?.status === "active" &&
      correction.analysis.analysis_generated_at !==
        chatRef?.analysis_generated_at,
  );
  const correctionActive =
    correction?.status === "active" && !correctionStale;
  const displayedAnalysis = correctionActive
    ? {
        ...analysis,
        root_cause: correction.revision.root_cause,
        suggested_fix: correction.revision.suggested_fix,
      }
    : analysis;
  const severityColor = severityAccent(displayedAnalysis.severity);

  async function revokeCorrection() {
    if (!correction || revokeBusy) return;
    const requestIdentity = analysisIdentity;
    revokeControllerRef.current?.abort();
    const controller = new AbortController();
    revokeControllerRef.current = controller;
    setRevokeBusy(true);
    setCorrectionError(null);
    try {
      await revokeAnalysisCorrection(correction.id, controller.signal);
    } catch (error) {
      if (
        !(error instanceof Error && error.name === "AbortError") &&
        identityRef.current === requestIdentity
      ) {
        setCorrectionError(
          error instanceof Error
            ? error.message
            : "Could not revoke the correction.",
        );
      }
    } finally {
      if (identityRef.current === requestIdentity) refetch();
      if (revokeControllerRef.current === controller) {
        revokeControllerRef.current = null;
        if (identityRef.current === requestIdentity) setRevokeBusy(false);
      }
    }
  }

  const statusRow = (
    <Stack
      direction="row"
      spacing={1}
      sx={{ alignItems: "center", flexWrap: "wrap", rowGap: 1 }}
    >
      {!detailAppearance && (
        <>
          <AutoAwesome sx={{ fontSize: 20, color: "primary.main" }} />
          <Typography variant="label" sx={{ fontWeight: 600 }} color="primary">
            AI analysis
          </Typography>
        </>
      )}
      {!severityInHeader && (
        <Chip
          size="small"
          label={`Severity: ${displayedAnalysis.severity}`}
          sx={(theme) => ({
            fontWeight: 600,
            ...(severityColor !== "primary"
              ? softChipSx(theme, severityColor)
              : { bgcolor: "action.selected", color: "text.secondary" }),
          })}
        />
      )}      {correctionActive && (
        <Chip
          size="small"
          color="success"
          variant="outlined"
          label="Maintainer corrected"
         
        />
      )}
      {correctionStale && (
        <Chip
          size="small"
          color="warning"
          variant="outlined"
          label="Correction stale"
         
        />
      )}
      {correction?.status === "revoked" && (
        <Chip
          size="small"
          variant="outlined"
          label="Correction revoked"
         
        />
      )}
    </Stack>
  );

  const correctionPanel = (
    <>
      {(correctionError || correctionsLoadError) && (
        <Alert severity="error" variant="outlined">
          {correctionError ?? correctionsLoadError}
        </Alert>
      )}
      {correction && (
        <Box
          sx={{
            border: "1px solid",
            borderColor: correctionActive ? "success.main" : "divider",
            borderRadius: 1,
            p: 1.25,
            bgcolor: detailAppearance
              ? "transparent"
              : (theme) =>
                  soft(
                    theme,
                    correctionActive ? "success" : "primary",
                    0.045,
                  ),
          }}
        >
          <Stack
            direction="row"
            spacing={1}
            sx={{ alignItems: "center", flexWrap: "wrap" }}
          >
            <PublishedWithChangesOutlined
              sx={{
                fontSize: 18,
                color: correctionActive ? "success.main" : "text.secondary",
              }}
            />
            <Typography variant="body2" sx={{ fontWeight: 650 }}>
              {correctionActive
                ? "Maintainer correction confirmed"
                : correctionStale
                  ? "This correction targets an older generated analysis and is not applied."
                  : "This correction was revoked and the original analysis is restored."}
            </Typography>
            <Button
              size="small"
              startIcon={<HistoryOutlined />}
              onClick={() => setShowOriginal((value) => !value)}
              sx={{ ml: { sm: "auto" } }}
            >
              {showOriginal
                ? "Hide details"
                : correctionActive
                  ? "View original"
                  : "View correction"}
            </Button>
            {correction.status === "active" &&
              features.analysis_corrections &&
              auth.status === "authenticated" && (
                <Button
                  size="small"
                  color="inherit"
                  startIcon={<UndoOutlined />}
                  onClick={() => void revokeCorrection()}
                  disabled={revokeBusy}
                 
                >
                  {revokeBusy ? "Revoking" : "Revoke"}
                </Button>
              )}
          </Stack>
          {showOriginal && (
            <Box
              sx={{
                mt: 1.25,
                pt: 1.25,
                borderTop: "1px solid",
                borderColor: "divider",
              }}
            >
              <Typography variant="caption" color="textSecondary">
                {correctionActive
                  ? "Original root cause"
                  : "Corrected root cause"}
              </Typography>
              <Typography
                variant="body2"
                sx={{ mt: 0.25, whiteSpace: "pre-line" }}
              >
                {correctionActive
                  ? analysis.root_cause
                  : correction.revision.root_cause}
              </Typography>
              <Typography
                variant="caption"
                color="textSecondary"
                sx={{ display: "block", mt: 1 }}
              >
                {correctionActive
                  ? "Original suggested remediation"
                  : "Corrected suggested remediation"}
              </Typography>
              <Typography
                variant="body2"
                sx={{ mt: 0.25, whiteSpace: "pre-line" }}
              >
                {correctionActive
                  ? analysis.suggested_fix
                  : correction.revision.suggested_fix}
              </Typography>
            </Box>
          )}
        </Box>
      )}
    </>
  );

  const dispositionPanel = analysis.disposition !== "grounded" ? (
    <Alert severity="warning" variant="outlined">
      Preliminary analysis. The structured result is safe to review, but evidence or
      quality checks remain unresolved. It cannot be used for corrections, remediation,
      actions, or fixes.
    </Alert>
  ) : null;

  const rootCause = detailAppearance ? (
    <BriefingSection label="Root cause">
      <RichText text={displayedAnalysis.root_cause} steps fileCtx={fileCtx} />
    </BriefingSection>
  ) : (
    <LabeledBlock label="Root cause" accent={severityColor}>
      <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
        <RichText text={displayedAnalysis.root_cause} steps fileCtx={fileCtx} />
      </Typography>
    </LabeledBlock>
  );

  const suggestedFix = detailAppearance ? (
    <BriefingSection label="Suggested remediation">
      <RichText text={displayedAnalysis.suggested_fix} steps fileCtx={fileCtx} />
    </BriefingSection>
  ) : (
    <LabeledBlock label="Suggested remediation" accent="primary">
      <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
        <RichText text={displayedAnalysis.suggested_fix} steps fileCtx={fileCtx} />
      </Typography>
    </LabeledBlock>
  );

  const files = analysis.relevant_files && analysis.relevant_files.length > 0 ? (
    detailAppearance ? (
      <BriefingSection label="Related files">
        <Stack spacing={0.5}>
          {[...analysis.relevant_files]
            .sort(
              (left, right) =>
                fileSortKey(left, fileCtx) - fileSortKey(right, fileCtx),
            )
            .map((file) => {
              const url = fileToUrl(file, fileCtx);
              return (
                <Box key={file} sx={{ ...overviewTypography.data, overflowWrap: "anywhere" }}>
                  {url ? (
                    <Link href={url} target="_blank" rel="noopener noreferrer" underline="hover">
                      {file}
                    </Link>
                  ) : (
                    <Box component="span" sx={{ color: "text.secondary" }}>
                      {file}
                    </Box>
                  )}
                </Box>
              );
            })}
        </Stack>
      </BriefingSection>
    ) : (
      <Box>
        <Typography
          variant="label"
          color="textSecondary"
          sx={{ fontWeight: 600, display: "block", mb: 0.5 }}
        >
          Related files
        </Typography>
        <Stack spacing={0.5}>
          {[...analysis.relevant_files]
            .sort(
              (left, right) =>
                fileSortKey(left, fileCtx) - fileSortKey(right, fileCtx),
            )
            .map((file) => {
              const url = fileToUrl(file, fileCtx);
              return (
                <Box
                  key={file}
                  sx={{
                    fontFamily: "monospace",
                    fontSize: "0.75rem",
                    overflowWrap: "anywhere",
                  }}
                >
                  {url ? (
                    <Link href={url} target="_blank" rel="noopener noreferrer" underline="hover">
                      {file}
                    </Link>
                  ) : (
                    <Box component="span" sx={{ color: "text.secondary" }}>
                      {file}
                    </Box>
                  )}
                </Box>
              );
            })}
        </Stack>
      </Box>
    )
  ) : null;

  const chat = chatRef ? (
    <AnalysisChat
      key={[
        chatRef.job_id,
        chatRef.build_id,
        chatRef.test_name,
        chatRef.source,
        chatRef.suite_name,
        chatRef.class_name,
        chatRef.junit_file,
        chatRef.analysis_generated_at,
      ].join("\u0000")}
      analysisRef={chatRef}
      fileCtx={fileCtx}
      fixPatterns={fixPatterns}
      onCorrectionChanged={refetch}
      appearance={detailAppearance ? "detail" : "default"}
    />
  ) : null;

  // An external cause explains why this analysis has no verified project file
  // and cannot start a fix proposal, so it belongs beside the remediation.
  const upstreamCause = externalCause(analysis.cause_location);
  const upstream = upstreamCause ? (
    detailAppearance ? (
      <BriefingSection label="Cause is in a dependency">
        <UpstreamCauseNotice location={upstreamCause} />
      </BriefingSection>
    ) : (
      <LabeledBlock label="Cause is in a dependency" accent="primary">
        <UpstreamCauseNotice location={upstreamCause} />
      </LabeledBlock>
    )
  ) : null;

  const content = (
    <Stack spacing={detailAppearance ? 2.25 : 2}>
      {statusRow}
      {dispositionPanel}
      {correctionPanel}
      {rootCause}
      {suggestedFix}
      {upstream}
      {/* A maintainer correction replaces the diagnosis, so the model's
          citations no longer necessarily support what is displayed. */}
      <EvidenceCitations
        citations={correctionActive ? undefined : analysis.evidence_citations}
        buildWebURL={buildWebURL}
        detailAppearance={detailAppearance}
      />
      {files}
      {chat}
      {traceRef && (
        <AnalysisTraceInspector
          key={`${traceRef.job_id}|${traceRef.build_id}|${traceRef.test_name}`}
          reference={traceRef}
        />
      )}
    </Stack>
  );

  if (detailAppearance) {
    return (
      <Box sx={{ minWidth: 0, maxWidth: "100%", overflowWrap: "anywhere" }}>
        {content}
      </Box>
    );
  }

  return (
    <Box
      component="section"
      className="ai-aurora"
      sx={{
        minWidth: 0,
        maxWidth: "100%",
        overflowWrap: "anywhere",
        borderRadius: 1,
        bgcolor: (theme) => soft(theme, "primary", 0.05),
        p: { xs: 2, sm: 2.5 },
      }}
    >
      {content}
    </Box>
  );
}
