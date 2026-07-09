// TypeScript module augmentation for custom palette keys and typography
// variants. This makes them available through `sx`, `theme.palette`, and the
// Typography `variant` prop.
import type * as React from "react";

// MUI's default palette only has background.default and background.paper; these
// fill in the remaining MD3 surface-container levels.
export interface SurfacePalette {
  main: string;
  dim: string;
  bright: string;
  container: string;
  containerLow: string;
  containerHigh: string;
  containerHighest: string;
  variant: string;
  tint: string;
  glass: string;
}

// Pass/fail colors for the run-history dot/bar visualizations, kept separate
// from the semantic success/error palette so they can be brightness-matched.
export interface DotPalette {
  pass: string;
  fail: string;
}

declare module "@mui/material/styles" {
  interface Palette {
    surface: SurfacePalette;
    dot: DotPalette;
  }
  interface PaletteOptions {
    surface?: Partial<SurfacePalette>;
    dot?: Partial<DotPalette>;
  }

  interface TypographyVariants {
    headline: React.CSSProperties;
    label: React.CSSProperties;
    data: React.CSSProperties;
    stat: React.CSSProperties;
  }
  interface TypographyVariantsOptions {
    headline?: React.CSSProperties;
    label?: React.CSSProperties;
    data?: React.CSSProperties;
    stat?: React.CSSProperties;
  }
}

declare module "@mui/material/Typography" {
  interface TypographyPropsVariantOverrides {
    headline: true;
    label: true;
    data: true;
    stat: true;
  }
}
