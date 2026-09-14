# Changelog

## 0.2.11

- Preserves ordered visual playback after successful runtime completion, including
  Results and the final step's full minimum display interval. Runtime completion
  and Results processing/persistence remain immediate.
- Prevents terminal frames, normal process exit and same-graph refresh from
  flushing the successful visual backlog. Automatic Results selection waits for
  playback, while failures, cancellation, blocked states and prompts still bypass it.
- Adds frame-sampled completion-with-backlog regressions at 200 and 500 ms,
  checking every dwell, immediate Results availability and stable graph geometry.

## 0.2.10

- Adds `yawr.preview.minimumStepDisplayMs` (integer, default 200, minimum 0)
  for editor-only live-step pacing; 0 disables it. Changes apply next run/session.
- Preserves ordered current-step transitions with commit-timed dwell intervals,
  while failures, blocked outcomes, prompts, cancellation and completion bypass
  the queue. Runtime execution and Results persistence remain immediate.
- Converges directly during reconnect and cancels pending pacing on hide/disposal.
  Reduced motion changes animation only, not the minimum display interval.

## 0.2.9

- Keeps one current execution marker across step boundaries, with a stable
  in-progress glow and distinct waiting, paused, blocked and terminal states.
- Moves execution activity into the inspector without removing diagnostics,
  per-step history or evidence.
- Preserves selection and execution geometry; offscreen steps use cancellable,
  zoom-preserving pans with reduced-motion support.
- Refreshes the bundled helper from the existing runtime source, including
  terminal Results support. No runtime protocol changes are required by this fix.

## 0.2.8

- Provides the Yawr runbook preview, graph, validation, authoring, highlighting,
  direct-run, investigation-session, route-test, and MCP bridge experiences.
- Uses only Yawr command, setting, chat, storage, protocol, environment, helper,
  package, and VSIX identities.
- Packages a verified Yawr runtime helper and deterministic `yawr-preview.vsix`.
