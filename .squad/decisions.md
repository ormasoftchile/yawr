# Squad Decisions

## Active Decisions

### 2026-09-11: Maintain Yawr-only identity and contracts

**By:** Cristián Ormazábal Ortega

**What:** Preserve implemented functionality while maintaining Yawr-only
identity and contracts. Do not retain compatibility aliases, historical
records, provenance artifacts, generated state, or repository memory tied to
any earlier product identity.

**Why:** The repository and its operational state must describe only the
current Yawr product.

### 2026-09-11: Centralize monorepo orchestration at the Yawr root

**By:** Lead

**What:** The repository root owns the sole npm lockfile and the
`apps/vscode` workspace, `go.work` references `./runtime`, and root scripts,
editor tasks, and CI address components through their monorepo paths. CI
builds each platform runtime helper explicitly for extension packaging.

**Why:** One reproducible dependency graph and path-correct orchestration keep
the monorepo coherent.

### 2026-09-11: Resolve extension build dependencies through the workspace

**By:** Lead

**What:** `apps/vscode/scripts/build-highlighting.mjs` resolves package roots
through Node module resolution rather than assuming a nested
`apps/vscode/node_modules`.

**Why:** npm workspaces may hoist dependencies to the repository root.

### 2026-09-11: Normalize authoritative VSIX archives

**By:** Lead

**What:** Yawr packaging rewrites the VSIX with sorted entries and fixed ZIP
metadata after `vsce package`.

**Why:** Release artifacts must be byte-reproducible.

### 2026-09-11: Claim mutable authoring workspaces by filesystem marker

**By:** Quality

**What:** Command authoring tests exclusively create a fixed ownership marker
inside each actual ProjectRoot. Isolation checks use shared and distinct roots
to prove ownership collision behavior.

**Why:** Filesystem-local exclusive creation proves writable workspace
identity without relying on names or scheduler order.

## Governance

- Record only current decisions the team has actually made.
- Preserve implemented functionality and maintain Yawr-only identity and
  contracts.
- Do not preserve superseded compatibility, migration, provenance, or
  historical records.
- Keep contracts separate from implementations.
- Fail closed, minimize blast radius, prove real production paths, and reject
  vacuous tests.
- Scribe owns durable decisions and concise agent context.
