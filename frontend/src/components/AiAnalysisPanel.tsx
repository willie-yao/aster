import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Chip from "@mui/material/Chip";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { AutoAwesome } from "@mui/icons-material";
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
  const detailAppearance = appearance === "detail";
  const severityColor = severityAccent(analysis.severity);

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
          label={`Severity: ${analysis.severity}`}
          sx={(theme) => ({
            fontWeight: 600,
            ...(severityColor !== "primary"
              ? softChipSx(theme, severityColor)
              : { bgcolor: "action.selected", color: "text.secondary" }),
          })}
        />
      )}
    </Stack>
  );

  const dispositionPanel = analysis.disposition !== "grounded" ? (
    <Alert severity="warning" variant="outlined">
      Preliminary analysis. The structured result is safe to review, but evidence or
      quality checks remain unresolved. It cannot directly authorize an action, although
      evidence-backed follow-up may still use its diagnosis.
    </Alert>
  ) : null;

  const advisorySemanticPanel = analysis.semantic_judge_mode === "advisory" &&
    (analysis.semantic_findings?.length ?? 0) > 0 ? (
      <Alert severity="info" variant="outlined">
        Semantic review noted unresolved concerns. The semantic finding itself is non-blocking,
        but other unresolved checks may still block an action.
      </Alert>
    ) : null;

  const rootCause = detailAppearance ? (
    <BriefingSection label="Root cause">
      <RichText text={analysis.root_cause} steps fileCtx={fileCtx} />
    </BriefingSection>
  ) : (
    <LabeledBlock label="Root cause" accent={severityColor}>
      <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
        <RichText text={analysis.root_cause} steps fileCtx={fileCtx} />
      </Typography>
    </LabeledBlock>
  );

  const suggestedFix = detailAppearance ? (
    <BriefingSection label="Suggested remediation">
      <RichText text={analysis.suggested_fix} steps fileCtx={fileCtx} />
    </BriefingSection>
  ) : (
    <LabeledBlock label="Suggested remediation" accent="primary">
      <Typography variant="body2" sx={{ whiteSpace: "pre-line", lineHeight: 1.6 }}>
        <RichText text={analysis.suggested_fix} steps fileCtx={fileCtx} />
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
      {advisorySemanticPanel}
      {rootCause}
      {suggestedFix}
      {upstream}
      <EvidenceCitations
        citations={analysis.evidence_citations}
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
