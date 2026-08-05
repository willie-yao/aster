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
