import Box from "@mui/material/Box";
import Chip, { type ChipProps } from "@mui/material/Chip";
import { statusToMuiColor, soft, accentLabelSx } from "../theme";

interface StatusChipProps extends Omit<ChipProps, "color" | "label"> {
  /** Dashboard status such as "PASSING", "FAILING", "FLAKY", or "passed". */
  status: string;
  /** Override the displayed text. Defaults to a sentence-case status. */
  label?: string;
}

function statusLabel(status: string): string {
  const normalized = status.trim().toLowerCase();
  return normalized ? normalized[0].toUpperCase() + normalized.slice(1) : status;
}

// Compact text and marker for a test or job status.
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
          }}
        />
      }
      label={label ?? statusLabel(status)}
      sx={[
        {
          height: 24,
          borderRadius: "4px",
          letterSpacing: 0,
          fontWeight: 600,
          fontSize: "0.6875rem",
          "& .MuiChip-icon": { ml: "7px", mr: "-2px" },
          "& .MuiChip-label": { px: 0.875 },
        },
        isDefault
          ? {
              bgcolor: "surface.containerHigh",
              color: "text.secondary",
              border: "1px solid",
              borderColor: "divider",
            }
          : (theme) => ({
              bgcolor: soft(theme, color, 0.1),
              ...accentLabelSx(theme, color),
              border: `1px solid ${soft(theme, color, 0.24)}`,
            }),
        ...(Array.isArray(sx) ? sx : [sx]),
      ]}
      {...rest}
    />
  );
}
