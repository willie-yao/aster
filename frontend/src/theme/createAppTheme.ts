import {
  createTheme,
  type Theme,
  type PaletteOptions,
} from "@mui/material/styles";
import "./augmentation";
import { darkTokens, lightTokens, type ColorTokens } from "./tokens";
import { buildComponents } from "./components";

function paletteFromTokens(t: ColorTokens): PaletteOptions {
  return {
    primary: {
      main: t.primary,
      dark: t.primaryDim,
      light: t.primaryContainer,
      contrastText: t.onPrimary,
    },
    success: {
      main: t.secondary,
      dark: t.secondaryDim,
      light: t.secondaryContainer,
      contrastText: t.onSecondary,
    },
    warning: {
      main: t.tertiary,
      light: t.tertiaryContainer,
      contrastText: t.onTertiary,
    },
    error: {
      main: t.error,
      dark: t.errorDim,
      light: t.errorContainer,
      contrastText: t.onError,
    },
    background: {
      default: t.background,
      paper: t.surfaceContainer,
    },
    text: {
      primary: t.onSurface,
      secondary: t.onSurfaceVariant,
    },
    divider: t.outlineVariant,
    dot: {
      pass: t.dotPass,
      fail: t.dotFail,
    },
    surface: {
      main: t.surface,
      dim: t.surfaceDim,
      bright: t.surfaceBright,
      container: t.surfaceContainer,
      containerLow: t.surfaceContainerLow,
      containerHigh: t.surfaceContainerHigh,
      containerHighest: t.surfaceContainerHighest,
      variant: t.surfaceVariant,
      tint: t.surfaceTint,
    },
  };
}

const monoFontFamily =
  'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace';

const typography = {
  fontFamily: 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
  htmlFontSize: 16,
  h1: { fontWeight: 700, letterSpacing: "-0.015em" },
  h2: { fontWeight: 650, letterSpacing: "-0.01em" },
  h3: { fontWeight: 650, letterSpacing: "-0.01em" },
  h4: {
    fontWeight: 700,
    fontSize: "1.5rem",
    lineHeight: 4 / 3,
    letterSpacing: "-0.015em",
  },
  h5: { fontWeight: 650, letterSpacing: "-0.01em" },
  h6: { fontWeight: 600, fontSize: "1rem", lineHeight: 1.5 },
  body1: { fontSize: "0.875rem", lineHeight: 10 / 7 },
  body2: { fontSize: "0.8125rem", lineHeight: 20 / 13 },
  caption: { fontSize: "0.75rem", lineHeight: 4 / 3 },
  button: { fontWeight: 600, fontSize: "0.8125rem" },
  headline: {
    fontFamily: "inherit",
    fontWeight: 600,
    fontSize: "1rem",
    lineHeight: 1.5,
    letterSpacing: "-0.005em",
  },
  label: {
    fontFamily: "inherit",
    fontWeight: 600,
    fontSize: "0.75rem",
    letterSpacing: 0,
    lineHeight: 4 / 3,
  },
  data: {
    fontFamily: monoFontFamily,
    fontWeight: 500,
    fontSize: "0.75rem",
    lineHeight: 4 / 3,
    letterSpacing: "-0.01em",
    fontFeatureSettings: '"tnum" 1, "cv01" 1',
  },
  stat: {
    fontFamily: monoFontFamily,
    fontWeight: 700,
    fontSize: "1.5rem",
    lineHeight: 4 / 3,
    letterSpacing: "-0.02em",
    fontFeatureSettings: '"tnum" 1',
  },
};

export function createAppTheme(
  tokens: { light: ColorTokens; dark: ColorTokens } = {
    light: lightTokens,
    dark: darkTokens,
  },
): Theme {
  return createTheme({
    cssVariables: { colorSchemeSelector: "class" },
    defaultColorScheme: "dark",
    colorSchemes: {
      light: { palette: paletteFromTokens(tokens.light) },
      dark: { palette: paletteFromTokens(tokens.dark) },
    },
    shape: { borderRadius: 6 },
    typography,
    components: buildComponents(),
  });
}
