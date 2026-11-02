import Button from "@mui/material/Button";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import type { ResolutionAvailability } from "../lib/resolvedState";

export function ResolutionStatus({ availability, onRetry }: {
  availability: ResolutionAvailability;
  onRetry: () => void;
}) {
  if (availability === "available") return null;
  return (
    <Stack direction="row" spacing={1} sx={{ alignItems: "center", flexWrap: "wrap" }}>
      <Typography variant="body2" color="textSecondary" role="status">
        {availability === "loading" ? "Resolution status is loading." : "Resolution status is unavailable."}
      </Typography>
      {availability === "unavailable" && <Button size="small" onClick={onRetry}>Retry resolution status</Button>}
    </Stack>
  );
}
