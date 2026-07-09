import Box from "@mui/material/Box";
import Chip, { type ChipProps } from "@mui/material/Chip";
import { statusToMuiColor, soft } from "../theme";

interface StatusChipProps extends Omit<ChipProps, "color" | "label"> {
  /** Dashboard status such as "PASSING", "FAILING", "FLAKY", or "passed". */
  status: string;
  /** Override the displayed text. Defaults to the status itself. */
  label?: string;
}

// Pill showing a test or job status with a leading color dot and themed colors.
export function StatusChip({ status, label, sx, ...rest }: StatusChipProps) {
  const color = statusToMuiColor(status);
  const isDefault = color === "default";
  return (
    <Chip
      size="small"
      icon={
        <Box
          component="span"
          sx={{
            width: 6,
            height: 6,
            borderRadius: "50%",
            flexShrink: 0,
            bgcolor: isDefault ? "text.secondary" : `${color}.main`,
            boxShadow: (t) => (isDefault ? "none" : `0 0 6px ${soft(t, color, 0.8)}`),
          }}
        />
      }
      label={label ?? status}
      sx={[
        {
          height: 22,
          textTransform: "uppercase",
          letterSpacing: "0.05em",
          fontWeight: 700,
          fontSize: "0.625rem",
          "& .MuiChip-icon": { ml: "8px", mr: "-2px" },
          "& .MuiChip-label": { px: 0.9 },
          ...(isDefault
            ? { bgcolor: "action.selected", color: "text.secondary" }
            : {
                bgcolor: (t) => soft(t, color, 0.15),
                color: `${color}.main`,
                border: (t) => `1px solid ${soft(t, color, 0.28)}`,
              }),
        },
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
      {...rest}
    />
  );
}
