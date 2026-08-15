import Box from "@mui/material/Box";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { CheckCircleOutlined, ErrorOutlined } from "@mui/icons-material";
import type { Theme } from "@mui/material/styles";
import { soft, type SoftColor } from "../theme";
import type { ActionPreview } from "../types/actions";

const sectionLabelSx = {
  display: "block",
  textTransform: "uppercase",
  fontSize: "0.625rem",
  fontWeight: 700,
  letterSpacing: "0.06em",
  color: "text.secondary",
  mb: 0.75,
} as const;

const previewBoxSx = {
  borderRadius: "10px",
  border: "1px solid",
  borderColor: "divider",
  bgcolor: (theme: Theme) => (theme.vars ?? theme).palette.surface.containerLow,
  p: 1.75,
  fontFamily: "monospace",
  fontSize: "0.8125rem",
  lineHeight: 1.65,
  whiteSpace: "pre-wrap",
  wordBreak: "break-word",
} as const;

function stripDraftComments(value: string): string {
  return value.replace(/<!--[\s\S]*?-->/g, "").trim();
}

function VerifyBadge({
  status,
  summary,
}: {
  status?: string;
  summary?: string;
}) {
  if (status !== "passed" && status !== "failed") return null;
  const passed = status === "passed";
  const accent: SoftColor = passed ? "success" : "error";
  return (
    <Box
      sx={{
        display: "flex",
        alignItems: "center",
        gap: 1,
        borderRadius: "10px",
        border: "1px solid",
        borderColor: (theme) => soft(theme, accent, 0.3),
        bgcolor: (theme) => soft(theme, accent, 0.12),
        px: 1.5,
        py: 1,
      }}
    >
      {passed ? (
        <CheckCircleOutlined
          sx={{ fontSize: 18, color: `${accent}.main`, flexShrink: 0 }}
        />
      ) : (
        <ErrorOutlined
          sx={{ fontSize: 18, color: `${accent}.main`, flexShrink: 0 }}
        />
      )}
      <Typography variant="body2" sx={{ wordBreak: "break-word" }}>
        <Box component="span" sx={{ fontWeight: 600, color: `${accent}.main` }}>
          {passed
            ? "Automated verification passed"
            : "Automated verification failed"}
        </Box>
        {summary && (
          <Box component="span" sx={{ color: "text.secondary" }}>
            {" · "}
            {summary}
          </Box>
        )}
        {!passed && (
          <Box component="span" sx={{ color: "text.secondary" }}>
            {". The change likely does not build as-is; treat it as a lead."}
          </Box>
        )}
      </Typography>
    </Box>
  );
}

export function ActionDraftPreview({ preview }: { preview: ActionPreview }) {
  return (
    <Stack spacing={2.5}>
      <Box>
        <Typography sx={sectionLabelSx}>Title</Typography>
        <Box
          sx={{
            borderRadius: "10px",
            border: "1px solid",
            borderColor: "divider",
            bgcolor: (theme) =>
              (theme.vars ?? theme).palette.surface.containerLow,
            px: 1.75,
            py: 1.25,
          }}
        >
          <Typography
            variant="body1"
            sx={{ fontWeight: 600, wordBreak: "break-word" }}
          >
            {preview.title}
          </Typography>
        </Box>
      </Box>

      {preview.kind === "fix" && (
        <Stack spacing={1.25}>
          <VerifyBadge
            status={preview.verify_status}
            summary={preview.verify_summary}
          />
          {preview.verify_status === "failed" && preview.verify_output && (
            <Box>
              <Typography sx={sectionLabelSx}>Verification output</Typography>
              <Box
                component="pre"
                sx={{
                  ...previewBoxSx,
                  m: 0,
                  maxHeight: 200,
                  overflow: "auto",
                }}
              >
                {preview.verify_output}
              </Box>
            </Box>
          )}
        </Stack>
      )}

      <Box>
        <Typography sx={sectionLabelSx}>
          {preview.kind === "fix" ? "Description" : "Body"}
        </Typography>
        <Box sx={{ ...previewBoxSx, maxHeight: 340, overflowY: "auto" }}>
          {stripDraftComments(preview.body) || "(no description)"}
        </Box>
      </Box>

      {preview.diff && (
        <Box>
          <Typography sx={sectionLabelSx}>Proposed diff</Typography>
          <Box
            component="pre"
            sx={{
              ...previewBoxSx,
              m: 0,
              maxHeight: 320,
              overflow: "auto",
            }}
          >
            {preview.diff}
          </Box>
        </Box>
      )}
    </Stack>
  );
}
