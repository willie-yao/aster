import Box from "@mui/material/Box";
import CircularProgress from "@mui/material/CircularProgress";

interface LoadingStateProps {
  /** Vertical padding in theme spacing units. Defaults to a tall, centered area. */
  py?: number;
}

// Centered spinner with configurable vertical padding.
export function LoadingState({ py = 16 }: LoadingStateProps) {
  return (
    <Box
      sx={{
        display: "flex",
        justifyContent: "center",
        alignItems: "center",
        py,
      }}
    >
      <CircularProgress />
    </Box>
  );
}
