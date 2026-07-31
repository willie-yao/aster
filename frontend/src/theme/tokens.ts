// Single source of truth for raw color values. Change a color once here and it
// propagates through the MUI theme. Each scheme exposes the same Material Design
// 3 token keys. To add a theme, create another ColorTokens object and wire it up
// in themes.ts.

export interface ColorTokens {
  background: string;
  surface: string;
  surfaceDim: string;
  surfaceBright: string;
  surfaceContainer: string;
  surfaceContainerLow: string;
  surfaceContainerHigh: string;
  surfaceContainerHighest: string;
  surfaceVariant: string;

  onSurface: string;
  onSurfaceVariant: string;

  primary: string;
  primaryDim: string;
  primaryContainer: string;
  onPrimary: string;

  // PASSING green maps to MUI success.
  secondary: string;
  secondaryDim: string;
  secondaryContainer: string;
  onSecondary: string;

  // FLAKY amber maps to MUI warning.
  tertiary: string;
  tertiaryContainer: string;
  onTertiary: string;

  error: string;
  errorDim: string;
  errorContainer: string;
  onError: string;

  // Pass/fail dot colors for run visualizations. Equal perceived brightness
  // keeps one dot from appearing larger than the other on dark surfaces.
  dotPass: string;
  dotFail: string;

  outline: string;
  outlineVariant: string;

  surfaceTint: string;

  // Translucent panel background. Stored pre-baked with alpha per scheme so it
  // switches correctly without runtime alpha math on a CSS variable.
  glass: string;
}

// Dark palette: layered slate surfaces with vibrant status hues. Near-black
// slate keeps glass panels legible while their backdrop-blur reads against it.
export const darkTokens: ColorTokens = {
  background: "#090c14",
  surface: "#090c14",
  surfaceDim: "#06080e",
  surfaceBright: "#1b2233",
  surfaceContainer: "#121826",
  surfaceContainerLow: "#0d111b",
  surfaceContainerHigh: "#1a2233",
  surfaceContainerHighest: "#212a40",
  surfaceVariant: "#212a40",

  onSurface: "#f1f5f9",
  onSurfaceVariant: "#93a1b8",

  primary: "#6d8bff",
  primaryDim: "#3b6fe0",
  primaryContainer: "#9fb6ff",
  onPrimary: "#04122e",

  secondary: "#34e39b",
  secondaryDim: "#12b981",
  secondaryContainer: "#065f46",
  onSecondary: "#00120a",

  tertiary: "#ffb020",
  tertiaryContainer: "#f59e0b",
  onTertiary: "#3d2800",

  error: "#ff5d6b",
  errorDim: "#e02f45",
  errorContainer: "#7f1d2b",
  onError: "#2b0007",

  // Brightness-matched so a lone failed dot among passes does not read as
  // smaller or higher on the dark surface.
  dotPass: "#2ee6a0",
  dotFail: "#ff6b78",

  outline: "#3a465e",
  outlineVariant: "#242e44",

  surfaceTint: "#6d8bff",

  glass: "rgba(18, 24, 38, 0.72)",
};

// Light palette: warm ivory foundations with muted lavender surface layers.
// Status colors stay dark enough for AA contrast across the softer surfaces.
export const lightTokens: ColorTokens = {
  background: "#f2efe8",
  surface: "#f2efe8",
  surfaceDim: "#d8d6df",
  surfaceBright: "#faf7f1",
  surfaceContainer: "#ebe9f0",
  surfaceContainerLow: "#efecf0",
  surfaceContainerHigh: "#e1e1eb",
  surfaceContainerHighest: "#d8d8e4",
  surfaceVariant: "#dcdbe7",

  onSurface: "#242333",
  onSurfaceVariant: "#59576a",

  primary: "#5d63b3",
  primaryDim: "#464b91",
  primaryContainer: "#d8dcf4",
  onPrimary: "#ffffff",

  secondary: "#0b7355",
  secondaryDim: "#075b44",
  secondaryContainer: "#bee3d2",
  onSecondary: "#ffffff",

  tertiary: "#9d4f0c",
  tertiaryContainer: "#f0d7b3",
  onTertiary: "#ffffff",

  error: "#c73535",
  errorDim: "#a52222",
  errorContainer: "#efc7c8",
  onError: "#ffffff",

  // Light scheme renders dark dots on a light surface, so no bloom mismatch;
  // keep the semantic pass/fail hues.
  dotPass: "#0b7355",
  dotFail: "#c73535",

  outline: "#757285",
  outlineVariant: "#c8c4d0",

  surfaceTint: "#5d63b3",

  glass: "rgba(235, 233, 240, 0.88)",
};
