import { AutoAwesome } from "@mui/icons-material";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Stack from "@mui/material/Stack";
import type { ReactNode } from "react";
import type { AnalysisChatReference } from "../types/analysisChat";
import type { AIAnalysis, PatternAnalysis } from "../types/dashboard";
import { firstSentence } from "../lib/testDetail";
import type { FileToUrlContext } from "../lib/utils";
import { AiAnalysisPanel } from "./AiAnalysisPanel";
import { AnalysisBriefing, NextStepPreview } from "./AnalysisBriefing";
import type { AnalysisTraceReference } from "./AnalysisTraceInspector";
import { RichText } from "./RichText";

export function TestAnalysisBriefing({
  analysis,
  summary,
  fileCtx,
  buildWebURL,
  traceRef,
  chatRef,
  fixPatterns,
  title = "Analysis briefing",
  metadata,
  context,
  chatTitle,
}: {
  analysis: AIAnalysis;
  summary?: string;
  fileCtx: FileToUrlContext;
  buildWebURL?: string;
  traceRef?: AnalysisTraceReference;
  chatRef?: AnalysisChatReference;
  fixPatterns?: PatternAnalysis[];
  title?: string;
  metadata?: ReactNode;
  context?: ReactNode;
  chatTitle?: string;
}) {
  return (
    <AnalysisBriefing
      title={title}
      icon={<AutoAwesome aria-hidden sx={{ fontSize: 18, color: "primary.main" }} />}
      metadata={metadata ?? `${analysis.severity} severity`}
      mobileNotice={analysis.disposition !== "citations_verified" ? (
        <Alert severity="warning" variant="outlined">
          Preliminary analysis. Evidence or quality checks remain unresolved. Treat it as a hypothesis during investigation.
        </Alert>
      ) : undefined}
      mobileNextStep={analysis.suggested_fix.trim() ? (
        <NextStepPreview>
          <RichText text={analysis.suggested_fix} steps fileCtx={fileCtx} />
        </NextStepPreview>
      ) : undefined}
      summary={(
        <Stack spacing={1.25}>
          <Box>
            <RichText text={summary ?? firstSentence(analysis.root_cause)} fileCtx={fileCtx} />
          </Box>
          {context}
        </Stack>
      )}
      details={(
        <AiAnalysisPanel
          analysis={analysis}
          fileCtx={fileCtx}
          buildWebURL={buildWebURL}
          traceRef={traceRef}
          fixPatterns={fixPatterns}
          chatRef={chatRef}
          chatTitle={chatTitle}
          appearance="detail"
          severityInHeader
        />
      )}
    />
  );
}
