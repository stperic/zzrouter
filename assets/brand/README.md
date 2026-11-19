# zzRouter brand assets

The identity has two independent vector masters:

- `zz-mark.svg`: the shared ZZ symbol, kept identical across products.
- `router-wordmark.svg`: the Router name, redrawn as vector paths from the
  selected Option 16 reference. Its bold split lettering needs no font files.

`zzrouter-logo.svg` combines the two for light backgrounds. Files ending in
`-dark.svg` use light artwork for dark backgrounds. All have transparent
backgrounds. Keep their proportions and leave clear space around the artwork.

`zz-icon.svg` and its 512px PNG export put the shared mark on a dark rounded
square for app icons. The Windows client, node and launcher all use this mark
through `winres/icon.png` (48px) and `winres/icon16.png` (16px).

Edit the two masters, then regenerate the combined logos, dark variants and
icons from the repository root with Python 3 and ImageMagick available:

```sh
python3 assets/brand/export.py
```

When the checkout contains the browser console, the same command exports its
separate Router wordmarks, shared marks and combined logos. Console exports
retain the charcoal, terracotta and cream palette.

The exporter replaces `winres/generate_icon.go`. Existing Windows resource
manifests and `make winres` consume the generated PNGs directly.

For another product, reuse `zz-mark.svg` unchanged and pair it with that
product's own wordmark. Use the shared mark alone where space is limited.
