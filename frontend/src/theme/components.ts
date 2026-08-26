import type { Components, Theme } from "@mui/material/styles";

// Global component defaults that encode the dashboard look once. Shared
// primitives own component-specific surfaces such as the glass panel.
export function buildComponents(): Components<Theme> {
  return {
    MuiCssBaseline: {
      styleOverrides: {
        body: {
          WebkitFontSmoothing: "antialiased",
          MozOsxFontSmoothing: "grayscale",
        },
      },
    },
    MuiButtonBase: {
      styleOverrides: {
        // ButtonBase sets no font of its own, so a bare button falls back to
        // the user agent's Arial. Every button-derived control picks up the
        // surrounding family here; anything with its own typography, such as
        // Button or Chip, still sets its own on top.
        root: { font: "inherit" },
      },
    },
    MuiButton: {
      defaultProps: { disableElevation: true },
      styleOverrides: {
        root: {
          textTransform: "none",
          fontWeight: 600,
          "&.Mui-focusVisible": {
            outline: "2px solid",
            outlineColor: "var(--mui-palette-primary-main)",
            outlineOffset: 1,
          },
        },
      },
    },
    MuiIconButton: {
      styleOverrides: {
        root: {
          "&.Mui-focusVisible": {
            outline: "2px solid",
            outlineColor: "var(--mui-palette-primary-main)",
            outlineOffset: 1,
          },
        },
      },
    },
    MuiChip: {
      styleOverrides: {
        // Chip is the one primitive that hardcodes a pill instead of reading
        // shape.borderRadius, so it is squared here rather than per surface.
        root: ({ theme }) => ({ borderRadius: theme.shape.borderRadius }),
        label: { fontWeight: 600 },
      },
    },
    MuiTooltip: {
      defaultProps: { arrow: true },
    },
    MuiTypography: {
      defaultProps: {
        variantMapping: {
          headline: "h2",
          label: "span",
          data: "span",
          stat: "span",
        },
      },
    },
  };
}
