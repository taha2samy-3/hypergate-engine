# Hypergate documentation site

The documentation at <https://taha2samy-3.github.io/hypergate-engine/>, built with [Docusaurus 3](https://docusaurus.io/).

```bash
npm ci            # install dependencies
npm start         # dev server with live reload on http://localhost:3000/hypergate-engine/
npm run build     # production build into build/ (fails on broken links)
npm run typecheck # TypeScript check of the site code
```

The same commands are available from the repository root as `task docs:dev`, `task docs:build` and `task docs:serve`.
Pushing changes under `website/` to `main` deploys the site to GitHub Pages (`.github/workflows/deploy-docs.yaml`).

## Layout

| Path | Content |
| --- | --- |
| `docs/` | Documentation pages. The sidebar is generated from this folder (`sidebar_position`, `_category_.json`). |
| `static/img/diagrams/` | Explanatory SVG diagrams used by the docs. They draw their own background so they read in light and dark mode. |
| `static/img/icons/` | 24px stroke icons (`currentColor`) for filters and concepts. |
| `static/img/logo.svg`, `logo-wordmark.svg`, `favicon.svg` | Brand mark and wordmark. |
| `src/pages/index.tsx`, `src/components/` | Landing page. |
| `src/css/custom.css` | Theme (brand colours: indigo `#6366F1`, cyan `#22D3EE`, navy `#0B1026`). |
