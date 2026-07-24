import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Divider from "@mui/material/Divider";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { FactCheckOutlined, PublishedWithChangesOutlined } from "@mui/icons-material";
import type { AnalysisCorrectionPreview } from "../types/corrections";
import { soft } from "../theme";

export function AnalysisCorrectionDialog({ preview, open, busy, error, onClose, onConfirm }: {
  preview: AnalysisCorrectionPreview | null;
  open: boolean;
  busy: boolean;
  error: string | null;
  onClose: () => void;
  onConfirm: () => void;
}) {
  if (!preview) return null;
  return (
    <Dialog open={open} onClose={busy ? undefined : onClose} fullWidth maxWidth="md">
      <DialogTitle sx={{ display: "flex", alignItems: "center", gap: 1 }}>
        <PublishedWithChangesOutlined color="warning" />
        Confirm analysis correction
      </DialogTitle>
      <DialogContent>
        {error && <Alert severity="error" variant="outlined" sx={{ mb: 2 }}>{error}</Alert>}
        <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
          This publishes a dashboard overlay. The original generated analysis remains preserved and can be restored.
        </Typography>
        <Stack spacing={2}>
          <Box>
            <Typography variant="label" color="text.secondary">Original root cause</Typography>
            <Typography variant="body2" sx={{ mt: 0.5, whiteSpace: "pre-line" }}>{preview.original.root_cause}</Typography>
          </Box>
          <Box sx={{ border: "1px solid", borderColor: (theme) => soft(theme, "warning", 0.4), bgcolor: (theme) => soft(theme, "warning", 0.07), borderRadius: "10px", p: 1.5 }}>
            <Typography variant="label" color="warning.main">Corrected root cause</Typography>
            <Typography variant="body2" sx={{ mt: 0.5, whiteSpace: "pre-line" }}>{preview.proposed.root_cause}</Typography>
            <Divider sx={{ my: 1.5 }} />
            <Typography variant="label" color="warning.main">Corrected fix</Typography>
            <Typography variant="body2" sx={{ mt: 0.5, whiteSpace: "pre-line" }}>{preview.proposed.suggested_fix}</Typography>
          </Box>
          <Box>
            <Stack direction="row" spacing={0.75} sx={{ alignItems: "center", mb: 0.75 }}>
              <FactCheckOutlined sx={{ fontSize: 17, color: "success.main" }} />
              <Typography variant="label">Verified evidence</Typography>
            </Stack>
            <Stack spacing={0.75}>
              {preview.citations.map((citation, index) => (
                <Typography key={`${citation.path}-${index}`} variant="caption" color="text.secondary" sx={{ fontFamily: "monospace" }}>
                  {citation.path}{citation.line_start ? `, line ${citation.line_start}` : ""}: {citation.quote}
                </Typography>
              ))}
            </Stack>
          </Box>
        </Stack>
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2.5 }}>
        <Button onClick={onClose} disabled={busy}>Cancel</Button>
        <Button variant="contained" color="warning" onClick={onConfirm} disabled={busy}>
          {busy ? "Publishing correction" : "Publish correction"}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
