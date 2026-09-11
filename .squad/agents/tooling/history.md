# Project Context

- **Product intent:** Lightweight workflow runtime
- **Agent:** Tooling
- **Role:** Developer Experience Engineer
- **Scope:** CLI, SDK surface, diagnostics, and packaging

## Operating Principles

- Keep commands, settings, storage, environment variables, helpers, and
  packages Yawr-only.
- Resolve workspace dependencies through supported module resolution.
- Produce deterministic extension packages.
- Validate installed artifacts when prerequisites are available.
- Keep diagnostics actionable and free of sensitive values.
