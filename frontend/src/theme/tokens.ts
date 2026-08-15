// Raw colors for the light and dark schemes. Both schemes expose the same
// semantic surface and status keys through the MUI theme.
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

  secondary: string;
  secondaryDim: string;
  secondaryContainer: string;
  onSecondary: string;

  tertiary: string;
  tertiaryDim: string;
  tertiaryContainer: string;
  onTertiary: string;

  error: string;
  errorDim: string;
  errorContainer: string;
  onError: string;

  dotPass: string;
  dotFail: string;

  outline: string;
  outlineVariant: string;
  surfaceTint: string;
}

export const darkTokens: ColorTokens = {
  background: "#0d1117",
  surface: "#0d1117",
  surfaceDim: "#090c10",
  surfaceBright: "#30363d",
  surfaceContainer: "#161b22",
  surfaceContainerLow: "#11161d",
  surfaceContainerHigh: "#1c2128",
  surfaceContainerHighest: "#21262d",
  surfaceVariant: "#21262d",

  onSurface: "#e6edf3",
  onSurfaceVariant: "#8b949e",

  primary: "#2f81f7",
  primaryDim: "#388bfd",
  primaryContainer: "#58a6ff",
  onPrimary: "#0d1117",

  secondary: "#3fb950",
  secondaryDim: "#2ea043",
  secondaryContainer: "#1a7f37",
  onSecondary: "#0d1117",

  tertiary: "#d29922",
  tertiaryDim: "#a9791b",
  tertiaryContainer: "#9e6a03",
  onTertiary: "#0d1117",

  error: "#f85149",
  errorDim: "#da3633",
  errorContainer: "#b62324",
  onError: "#ffffff",

  dotPass: "#3fb950",
  dotFail: "#f85149",

  outline: "#8c959f",
  outlineVariant: "#30363d",
  surfaceTint: "#2f81f7",
};

export const lightTokens: ColorTokens = {
  background: "#f6f8fa",
  surface: "#ffffff",
  surfaceDim: "#d8dee4",
  surfaceBright: "#ffffff",
  surfaceContainer: "#ffffff",
  surfaceContainerLow: "#f6f8fa",
  surfaceContainerHigh: "#f0f3f6",
  surfaceContainerHighest: "#eaeef2",
  surfaceVariant: "#eaeef2",

  onSurface: "#1f2328",
  onSurfaceVariant: "#59636e",

  primary: "#0969da",
  primaryDim: "#0550ae",
  primaryContainer: "#54aeff",
  onPrimary: "#ffffff",

  secondary: "#1a7f37",
  secondaryDim: "#116329",
  secondaryContainer: "#dafbe1",
  onSecondary: "#ffffff",

  tertiary: "#9a6700",
  tertiaryDim: "#7d4e00",
  tertiaryContainer: "#fff8c5",
  onTertiary: "#ffffff",

  error: "#cf222e",
  errorDim: "#a40e26",
  errorContainer: "#ffebe9",
  onError: "#ffffff",

  dotPass: "#1a7f37",
  dotFail: "#cf222e",

  outline: "#57606a",
  outlineVariant: "#d0d7de",
  surfaceTint: "#0969da",
};
