# Brand

Aster is a proper name. Its operating-model expansion is **Automated Signal
Triage, Explanation, and Remediation**. [`README.md`](../README.md) is the source
of truth for the public tagline and product description.

Do not describe Aster as autonomous repair, self-healing, or guaranteed
root-cause detection.

## Mark

The mark is a forward-facing prow whose counter forms a capital A. It is drawn as
a single path with `fill-rule="evenodd"`, so the counter is a real hole rather
than a background-colored shape.

```
d="M32 5.1 60 58.9 32 49.02 4 58.9Z M32 18.32 40.78 38.22 32 34.1 23.22 38.22Z"
```

The fill is the brand gradient, violet to pink on a `x2="0.35" y2="1"` diagonal.
Keep the artwork on a `0 0 64 64` viewBox so proportions stay stable across
sizes. Below roughly 20px the gradient reads as a flat mid-tone, which is
expected and needs no separate flat asset.

The counter shows whatever is behind it, so the mark needs no container, plate,
or border. It still needs contrast against its surface: on brand-colored or
photographic backgrounds, drop the gradient and set a flat white `fill` rather
than placing a violet mark on a violet field.

Do not add an inner glyph. The mark is legible down to 16px only because the
counter stays open.

## Color

Brand and status colors do different jobs. The brand ramp is violet to pink; the
status colors stay green, amber, and red because they carry CI meaning. Never
restyle a pass/fail indicator to match the brand.

Use violet for navigation and buttons so brand chrome remains visually distinct
from green pass and red failure states.

The canonical values live in
[`frontend/src/theme/tokens.ts`](../frontend/src/theme/tokens.ts); treat that
file as authoritative and mirror changes here.

### Brand ramp

| Stop | Light | Dark |
| --- | --- | --- |
| From (violet) | `#7c3aed` | `#a78bfa` |
| To (pink) | `#ec4899` | `#f472b6` |

Dark uses lightened stops so the mark holds contrast against `#0d1117`.

In the app the stops are the `brand.from` and `brand.to` palette keys, consumed
as the CSS variables `--mui-palette-brand-from` and `--mui-palette-brand-to`.
They exist for identity surfaces only and carry no status meaning.

The gradient is for the mark and for thin accents. Do not run it behind body
text: gradient text cannot be held to a contrast ratio.

### Interface

| Role | Light | Dark | Token |
| --- | --- | --- | --- |
| Primary | `#7c3aed` | `#a78bfa` | `primary` |
| Primary hover | `#6d28d9` | `#c4b5fd` | `primaryDim` |
| Primary tint | `#c4b5fd` | `#ddd6fe` | `primaryContainer` |
| Text | `#1f2328` | `#e6edf3` | `onSurface` |
| Muted text | `#59636e` | `#8b949e` | `onSurfaceVariant` |
| Surface | `#ffffff` | `#0d1117` | `surface` |
| Pass | `#1a7f37` | `#3fb950` | `dotPass` |
| Fail | `#cf222e` | `#f85149` | `dotFail` |

Every text-bearing color above clears WCAG AA (4.5:1) on its own surface:
primary reaches 5.70:1 light and 6.95:1 dark, and primary hover 7.10:1 and
10.25:1. `primaryContainer` is a tint for fills and borders, not text; at 1.85:1
on white it must never carry a label.

Pink is absent from this table on purpose. `#ec4899` scores 3.53:1 on white and
fails AA as text, so it is a gradient endpoint only and never a link, label, or
button color.

## Typography

Brand surfaces use the system UI stack, matching the app's `typography.fontFamily`.
The wordmark is weight 800 at roughly `-0.042em` tracking. Monospace is reserved
for identifiers, paths, and log excerpts.

## Assets

| File | Use |
| --- | --- |
| [`assets/aster-mark.svg`](assets/aster-mark.svg) | Canonical mark, light-scheme gradient |
| [`../frontend/public/favicon.svg`](../frontend/public/favicon.svg) | Favicon; switches fill on `prefers-color-scheme` |
| `assets/aster-banner-light.png` | README banner, light |
| `assets/aster-banner-dark.png` | README banner, dark |
| [`assets/banner-src.html`](assets/banner-src.html) | Banner source |
| [`../frontend/src/components/Layout.tsx`](../frontend/src/components/Layout.tsx) | In-app header mark |

## Regenerating the banner

The banner ships as PNG, not SVG. GitHub renders README images in an isolated
context that cannot load web fonts, so SVG text falls back to a different face on
every viewer's machine. Rasterizing pins the result.

Render at `--force-device-scale-factor=2` for a 2560x600 image, then display it
at 1280 CSS pixels:

```bash
cd docs/assets
chrome --headless --screenshot=aster-banner-light.png \
  --window-size=1280,300 --force-device-scale-factor=2 banner-src.html
chrome --headless --screenshot=aster-banner-dark.png \
  --window-size=1280,300 --force-device-scale-factor=2 'banner-src.html?mode=dark'
```

The dot field uses a seeded generator, so the layout is identical on every run.
Exact bytes are not portable: Chrome version, platform font rasterization, and
PNG encoder all affect output, so regenerate both files on one machine and commit
them as a pair. Keep every type size in the source at 18px or larger: GitHub's
content column is about 900px, which scales the 1280px banner to roughly 0.7x.
