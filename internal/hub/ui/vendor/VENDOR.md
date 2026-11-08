# Vendored libraries

Downloaded with curl on 2026-10-18; no npm, no bundler (DESIGN §17.5).

| File | Package | Version | URL | License |
|---|---|---|---|---|
| `preact.module.js` | preact | 10.29.8 | https://cdn.jsdelivr.net/npm/preact@10.29.8/dist/preact.module.js | MIT, `LICENSE-preact` (https://cdn.jsdelivr.net/npm/preact@10.29.8/LICENSE) |
| `hooks.module.js` | preact/hooks | 10.29.8 | https://cdn.jsdelivr.net/npm/preact@10.29.8/hooks/dist/hooks.module.js | MIT, `LICENSE-preact` |
| `htm.module.js` | htm | 3.1.1 | https://cdn.jsdelivr.net/npm/htm@3.1.1/dist/htm.module.js | Apache-2.0, `LICENSE-htm` (https://cdn.jsdelivr.net/npm/htm@3.1.1/LICENSE) |

Local changes, so the files load as plain ES modules without an import map (an
inline `<script type="importmap">` would need a CSP hash):

* `hooks.module.js`: `from"preact"` → `from"./preact.module.js"`.
* `preact.module.js`, `hooks.module.js`: the trailing `//# sourceMappingURL=` comment
  is removed (the maps are not vendored).

Upgrading: download the same paths at the new version, reapply the two edits, update
this table. `TestUIModulesHaveNoBareImports` fails if a bare specifier is left.
