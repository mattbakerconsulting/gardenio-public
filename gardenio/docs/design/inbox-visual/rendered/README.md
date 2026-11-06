# Rendered PNGs

Rasterized renders of the sibling `*.svg` files at 2× device scale, committed
so reviewers in viewers that do not render SVGs (GitLab MR diff thumbnails,
some local readers) can see the mockups.

The SVGs in the parent directory are the source of truth. To re-render after
edits:

```sh
node scripts/render-svgs.mjs \
  gardenio/docs/design/inbox-visual \
  gardenio/docs/design/inbox-visual/rendered
```

See `scripts/render-svgs.mjs` for details. Runs inside the agent container
out of the box (uses bundled Chromium); on a contributor laptop, set
`CHROME_PATH` to a local Chrome binary.
