# Yawr

Yawr is a monorepo for the workflow runtime and its VS Code extension.

- [`runtime/README.md`](runtime/README.md) — Go runtime and `yawr` CLI
- [`apps/vscode/README.md`](apps/vscode/README.md) — VS Code extension

## Prerequisites

- Node.js 20 or newer
- Go 1.25.7 (read from `runtime/go.mod`)

## Install

Install JavaScript dependencies once at the repository root:

```sh
npm ci
```

The root workspace owns `apps/vscode`; there is no component lockfile.

## Extension

```sh
npm run extension:compile
npm run extension:test
npm run extension:package
npm run extension:e2e
```

Before packaging on a new platform, build the matching runtime helper and pass
its path explicitly (the packaging command never falls back to `PATH`):

```sh
go build -C runtime -o yawr ./cmd/yawr
npm run extension:package-helper -- ./runtime/yawr
```

On Windows, use `runtime\yawr.exe` for both output and argument paths.

## Runtime

```sh
npm run runtime:build
npm run runtime:test
npm run runtime:build:yawr
```

`go.work` points at `./runtime`; the runtime module identity is
`github.com/ormasoftchile/yawr/runtime`. Projects use `.yawr/config.yaml`,
`yawr-package.yaml`, and `yawr-extension.yaml`.

## Combined checks

```sh
npm run check
```

This verifies root wiring, monorepo relocation, and the extension. Use
`npm run check:with-go` to include the runtime build and tests.

Extension-host tests are pinned to VS Code 1.137.0, matching the extension's
declared `^1.137.0` engine range.
