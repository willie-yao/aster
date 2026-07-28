import Box from "@mui/material/Box";
import LinearProgress from "@mui/material/LinearProgress";
import Paper from "@mui/material/Paper";
import Typography from "@mui/material/Typography";
import { fetchStatusPresentation, formatFetchTimestamp, nextFetchTime } from "../lib/fetchStatus";
import { useFetchStatus } from "../hooks/useFetchStatus";
import { soft } from "../theme";

export function FetchProgressBanner() {
  const response = useFetchStatus();
  const presentation = response ? fetchStatusPresentation(response) : null;
  const status = response?.status;
  if (!presentation || !status) return null;

  const progress = presentation.determinateTotal && presentation.determinateTotal > 0
    ? Math.min(100, (presentation.determinateCompleted / presentation.determinateTotal) * 100)
    : null;
  const schedule = response.state === "idle" ? nextFetchTime(status) : null;

  return (
    <Box sx={{ px: { xs: 2, sm: 3 }, pt: 2, width: "100%", minWidth: 0 }}>
      <Paper
        role="status"
        aria-live="polite"
        aria-label={presentation.ariaLabel}
        variant="outlined"
        sx={{
          mx: "auto",
          maxWidth: "xl",
          minWidth: 0,
          overflow: "hidden",
          borderColor: `${presentation.severity}.main`,
          bgcolor: (theme) => soft(theme, presentation.severity, 0.06),
        }}
      >
        <Box
          sx={{
            display: "grid",
            gridTemplateColumns: { xs: "minmax(0, 1fr)", md: "minmax(0, 1fr) auto" },
            gap: { xs: 0.75, md: 2 },
            alignItems: "center",
            px: { xs: 1.5, sm: 2 },
            py: 1.25,
            minWidth: 0,
          }}
        >
          <Box sx={{ minWidth: 0 }}>
            <Typography variant="subtitle2" sx={{ fontWeight: 700, overflowWrap: "anywhere" }}>
              {presentation.title}
            </Typography>
            <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: "anywhere" }}>
              {presentation.detail}
            </Typography>
            {schedule && (
              <Typography variant="caption" color="text.secondary" sx={{ display: "block", mt: 0.25, overflowWrap: "anywhere" }}>
                {schedule}
              </Typography>
            )}
          </Box>
          <Box sx={{ minWidth: 0, textAlign: { xs: "left", md: "right" } }}>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
              Last checked: {formatFetchTimestamp(status.last_checked_at)}
            </Typography>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
              Last published: {formatFetchTimestamp(status.last_successful_publication_at)}
            </Typography>
          </Box>
        </Box>
        {response.state === "active" && (
          <LinearProgress
            aria-label="Fetch progress"
            variant={progress === null ? "indeterminate" : "determinate"}
            value={progress ?? undefined}
            sx={{ height: 3 }}
          />
        )}
      </Paper>
    </Box>
  );
}
