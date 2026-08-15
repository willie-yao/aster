import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Typography from "@mui/material/Typography";

interface ErrorStateProps {
  title?: string;
  message?: string;
  /** Retry handler; if omitted, no button is shown. */
  onRetry?: () => void;
}

// Centered error message with an optional retry button.
export function ErrorState({
  title = "Something went wrong",
  message,
  onRetry,
}: ErrorStateProps) {
  return (
    <Box
      sx={{
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        gap: 2,
        py: 16,
        textAlign: "center",
      }}
    >
      <Typography variant="h6" color="error">
        {title}
      </Typography>
      {message && (
        <Typography variant="body2" color="text.secondary">
          {message}
        </Typography>
      )}
      {onRetry && (
        <Button variant="contained" onClick={onRetry}>
          Retry
        </Button>
      )}
    </Box>
  );
}
