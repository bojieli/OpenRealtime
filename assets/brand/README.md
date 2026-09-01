# OpenRealtime brand assets

The mark is a compact graph: one explicit **Tee**, two replaceable lanes, and
one explicit **Mux**. The lanes form the `O` in OpenRealtime while the angular
channel shape keeps the silhouette distinct from a generic neural-network,
waveform, or orbit logo.

## Primary files

| File | Use |
| --- | --- |
| `openrealtime-mark.svg` | Primary transparent vector mark on light backgrounds |
| `openrealtime-mark-mono.svg` | One-ink vector mark for print and constrained uses |
| `openrealtime-mark-reverse.svg` | Vector mark for dark backgrounds |
| `openrealtime-logo-horizontal.svg` | Primary outlined horizontal lockup; no font dependency |
| `openrealtime-app-icon.svg` | Square dark-background application icon source |
| `openrealtime-favicon.svg` | Optically simplified small-size browser icon |

PNG exports are included for immediate use: a 1024 px mark and app icon, a
1920 px-wide horizontal lockup, and 16/32/48 px favicon sizes. `favicon.ico`
contains all three favicon resolutions.

## Palette

- Graph ink: `#101828`
- Live signal: `#12BFA7`
- White: `#FFFFFF`
- Neutral presentation canvas: `#F4F6F9`

## Usage

- Prefer the primary mark or outlined horizontal lockup on white and light
  neutral surfaces.
- Use the reverse mark on graph ink or similarly dark surfaces.
- Keep clear space around the mark equal to at least half the height of one
  endpoint block.
- Use the full mark at 32 px or larger. Below 32 px, use
  `openrealtime-favicon.svg`, whose internal Tee/Mux detail is intentionally
  removed for legibility.
- Do not rotate the mark, recolor individual lanes, add gradients, or place it
  inside another outline.

`openrealtime-logo-horizontal-source.svg` retains editable text. The shipped
`openrealtime-logo-horizontal.svg` converts that lettering to paths so it
renders consistently without an installed font.
