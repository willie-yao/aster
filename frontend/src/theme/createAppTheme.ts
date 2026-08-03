import {
  createTheme,
  type Theme,
  type PaletteOptions,
} from "@mui/material/styles";
import "./augmentation";
import { darkTokens, lightTokens, type ColorTokens } from "./tokens";
import { buildComponents } from "./components";

// Map raw MD3 tokens onto MUI palette slots. Semantic mapping:
//   primary blue tokens -> primary
//   secondary green PASSING tokens -> success
//   tertiary amber FLAKY tokens -> warning
//   error red tokens -> error
// The extra surface-container levels live under the custom `surface` key.
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
      glass: t.glass,
    },
  };
}

const monoFontFamily =
  'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace';

const typography = {
  fontFamily: 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
  // The root html font-size is 17px in index.css. Keeping MUI's htmlFontSize at
  // 16 preserves the dashboard's rem scale.
  htmlFontSize: 16,
  h1: { fontWeight: 800, letterSpacing: "-0.02em" },
  h2: { fontWeight: 700, letterSpacing: "-0.01em" },
  h3: { fontWeight: 700, letterSpacing: "-0.01em" },
  // Page titles and stat counts.
  h4: { fontWeight: 800, fontSize: "1.75rem", letterSpacing: "-0.02em" },
  h5: { fontWeight: 700, letterSpacing: "-0.01em" },
  // Sub-section headings and empty/error titles.
  h6: { fontWeight: 600, fontSize: "1.125rem" },
  button: { fontWeight: 600 },
  // Custom variants for reusable section titles and compact labels. Call sites
  // override fontSize via `sx` for larger titles or smaller card titles.
  headline: {
    fontFamily: "inherit",
    fontWeight: 700,
    fontSize: "1.125rem",
    letterSpacing: "-0.01em",
  },
  label: {
    fontFamily: "inherit",
    fontWeight: 600,
    fontSize: "0.6875rem",
    letterSpacing: "0.05em",
    lineHeight: 1.4,
  },
  // Monospace face for data values: build IDs, durations, percentages, counts.
  // Tabular figures keep columns from shifting as digits change.
  data: {
    fontFamily: monoFontFamily,
    fontWeight: 500,
    fontSize: "0.8125rem",
    letterSpacing: "-0.01em",
    fontFeatureSettings: '"tnum" 1, "cv01" 1',
  },
  // Large metric numbers for the summary strip and stat rows.
  stat: {
    fontFamily: monoFontFamily,
    fontWeight: 700,
    fontSize: "1.75rem",
    lineHeight: 1.1,
    letterSpacing: "-0.02em",
    fontFeatureSettings: '"tnum" 1',
  },
};

// Build the dashboard theme. Light and dark schemes are generated from token
// sets and switched at runtime by the class selector used by useColorScheme in
// the app shell. To create another theme, pass different token sets or register
// a new factory in themes.ts.
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
    shape: { borderRadius: 12 },
    typography,
    components: buildComponents(),
  });
}
