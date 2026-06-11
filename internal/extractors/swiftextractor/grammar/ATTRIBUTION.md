# Third-party attribution: tree-sitter Swift grammar

This directory vendors the **tree-sitter Swift grammar** so the Swift extractor can
parse Swift source into an AST. It is third-party code, distributed under a
different license (MIT) than the rest of enola (Apache-2.0).

## Source

- **Project:** [tree-sitter-swift](https://github.com/alex-pinkus/tree-sitter-swift)
- **Author:** Alex Pinkus <alex.pinkus@gmail.com>
- **License:** MIT — see [LICENSE](./LICENSE) (Copyright (c) 2021 alex-pinkus)
- **Version vendored:** Go module `github.com/alex-pinkus/tree-sitter-swift`
  pseudo-version `v0.0.0-20260601025047-d42e9bb24646` (commit `d42e9bb24646`)

## What is vendored, and how it was produced

| File(s) | Origin |
|---|---|
| `src/scanner.c` | Copied verbatim from the upstream repository (the project's hand-written external scanner). |
| `grammar.json`, `src/node-types.json` | Copied verbatim from the upstream repository. |
| `src/parser.c`, `src/tree_sitter/*.h` | **Generated** locally from `grammar.json` using the tree-sitter CLI. Upstream git-ignores the generated parser, so it must be regenerated to build. |

The generated parser is a deterministic product of the upstream `grammar.json`; it
was not hand-edited. The `cgo_parser.c` / `cgo_scanner.c` shims and `binding.go` in
this directory are enola's own glue code (Apache-2.0) and are not part of the
upstream grammar.

## Regenerating `src/parser.c`

```sh
cd internal/extractors/swiftextractor/grammar
tree-sitter generate grammar.json --abi 14
```

The ABI (14) is pinned to match `github.com/tree-sitter/go-tree-sitter v0.24.0`.
This was produced with `tree-sitter-cli` v0.24.7 (e.g. `npx tree-sitter-cli@0.24.7`).
