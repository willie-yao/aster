import { createAppTheme } from "./createAppTheme";

// Named-theme registry. The app shell reads `defaultTheme`; extend this to
// support additional themes.
export const themes = {
  default: createAppTheme(),
} as const;

export type ThemeName = keyof typeof themes;

export const defaultTheme = themes.default;
