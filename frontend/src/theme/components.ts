import { collapseClasses } from "@mui/material/Collapse";
import { keyframes, type Components, type Theme } from "@mui/material/styles";

// Disclosed content eases in by opacity and transform, which the compositor
// handles without relayout.
const disclose = keyframes`
  from {
    opacity: 0;
    transform: translateY(-4px);
  }
`;

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
        root: {
          font: "inherit",
          // The ripple is decorative interaction motion. Press is already
          // carried by the hover and active surface change, so it is dropped
          // rather than left expanding for half a second.
          "@media (prefers-reduced-motion: reduce)": {
            "& .MuiTouchRipple-root": { display: "none" },
          },
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
        // Chip is the one primitive that hardcodes a pill instead of reading
        // shape.borderRadius, so it is squared here rather than per surface.
        root: ({ theme }) => ({ borderRadius: theme.shape.borderRadius }),
        label: { fontWeight: 600 },
      },
    },
    MuiTooltip: {
      defaultProps: { arrow: true },
    },
    // MUI animates layout properties by default, which relayouts the page on
    // every frame. Panels and labels change size at once instead.
    MuiCollapse: {
      styleOverrides: {
        // The panel snaps to its height; its content fades in while entering.
        root: {
          transition: "none",
          [`&:not(.${collapseClasses.entered}):not(.${collapseClasses.hidden}) > .${collapseClasses.wrapper} > .${collapseClasses.wrapperInner}`]: {
            animation: `${disclose} 180ms cubic-bezier(0.16, 1, 0.3, 1)`,
          },
          "@media (prefers-reduced-motion: reduce)": {
            [`& .${collapseClasses.wrapperInner}`]: { animation: "none" },
          },
        },
      },
    },
    MuiInputLabel: {
      styleOverrides: {
        // The floating label still moves by transform; its max-width snaps.
        animated: ({ theme }) => ({
          transition: theme.transitions.create(["color", "transform"], {
            duration: theme.transitions.duration.shorter,
            easing: theme.transitions.easing.easeOut,
          }),
        }),
      },
    },
    MuiOutlinedInput: {
      styleOverrides: {
        // The notch opens at once rather than animating the legend's width.
        notchedOutline: { "& legend": { transition: "none" } },
      },
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
