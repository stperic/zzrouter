# zzRouter brand assets

The symbol reads **ZZ**. Pair it with **Router**, never with another `zzRouter`
wordmark. Use the full product name `zzRouter` when writing without the symbol.
The console retains `zzRouter` as the accessible name of the complete lockup.

| Asset | Use |
| --- | --- |
| `mark-color.svg` | Charcoal and terracotta symbol on light backgrounds |
| `mark-dark.svg` | Cream and terracotta symbol on dark backgrounds |
| `mark-black.svg` | Solid black symbol for monochrome use |
| `mark-white.svg` | Solid white symbol for dark backgrounds |
| `app-icon.svg` | Browser favicon and square app icon, with its own terracotta background |
| `wordmark-color.svg` | Color symbol followed by Router on light backgrounds |
| `wordmark-dark.svg` | Color symbol followed by Router on dark backgrounds |
| `wordmark-black.svg` | Entire lockup in black |
| `wordmark-white.svg` | Entire lockup in white |

SVGs have transparent backgrounds except for the app icon. All marks share the
same three filled paths. The wordmark letters are outlines, so no font download
or installed font is required. Keep the shapes and their spacing together when
resizing. Use the app icon for browser tabs and the plain marks for page branding.

The main palette is charcoal `#242420`, terracotta `#d97757`, and cream `#faf9f5`.
The app tile uses deeper terracotta `#c96f4d` behind charcoal and cream strokes.
The console selects the light or dark mark through the existing theme tokens.

The terminal renditions live in `pkg/ui/brand.go`. CLI help uses the five-line
ASCII logo. The first TUI menu draws in the interlocking double-Z symbol beside
the Router wordmark. The terminal cells follow the console mark and keep the
terracotta stroke in both themes, with cream on dark and charcoal on light.
Opening a menu item or group replaces it with the single-line `zzRouter` wordmark,
which all working views share. Short and narrow terminals start compact.
Command names and machine-readable output retain the
`zzrouter` identity.
