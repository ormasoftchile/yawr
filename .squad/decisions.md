# Yawr Decisions

## Current Decisions

### Repository layout

The root owns the npm workspace and lockfile. `go.work` references `runtime`.
Root scripts, editor tasks, and CI use monorepo-relative component paths.

### Runtime and extension contracts

The runtime accepts only current Yawr schemas. Tool references use `name` plus
optional `package`, `version`, or `path`. Authoring capabilities require the
explicit `--v3` argument in source and packaged helpers.

### Packaging

VSIX archives use sorted entries and fixed ZIP metadata. Packaged helpers are
built from the matching runtime source and verified against source behavior
before packaging.

### Testing

Tests build executables in test-owned temporary directories. Validation must
leave tracked files unchanged and the worktree clean.

### Team

The active roster is Lead, Runtime, Tooling, Tester, Scribe, Ralph, Rai, and
Fact Checker. Squad state records only current Yawr project context.
