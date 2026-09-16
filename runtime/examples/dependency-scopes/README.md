# Included runbooks with file-local tools

These are verification fixtures for the 0.2.18 dependency-scope review candidate.
They require its matching runtime and extension; published YAWR 0.2.16 does not
support this contract. The candidate's VSIX bundles the required helper.

All packages are included here and pinned by relative paths. No package map,
credentials, external service, shell command, or operator input is required.
The two packages both export `query.read`, implemented by local runbook
substitutions. Each returns its own marker (`left` or `right`); assertions check
that the selected definition is correct.

| Entrypoint | Expected execution |
| --- | --- |
| `static-and-lazy.runbook.yaml` | Parent uses `left`, eager child uses `right`, lazy child uses `left`, parent still uses `left` |
| `parallel.runbook.yaml` | Siblings execute concurrently; each child's `query` resolves to its own package |
| `dynamic.runbook.yaml` | Catalog selection enters `left`, then `right`, then `left` again; each occurrence retains its own history |
| `dynamic-parallel.runbook.yaml` | Parallel branches dynamically select their children; graph identities retain the correct branch and tool scope |
| `dynamic-through-tool.runbook.yaml` | A frozen tool-backed runbook dynamically enters both children; all nested runbooks remain visible in history |

Each child declares its own `requires` and `toolRefs`. The parallel parent
declares neither. The dynamic parent requires only the child-runbook package;
preflight discovers the tool packages through the eligible child declarations.
The lazy example defers execution, not dependency validation or freezing.
The tool-backed router declares its own dependency on the child catalog and
uses the package source already selected by the entrypoint.

## Expected preflight failures

These deliberately invalid entrypoints must refuse execution, not finish an
investigation successfully:

| Entrypoint under `rejections` | Expected refusal |
| --- | --- |
| `parent-private-alias.runbook.yaml` | The child has no private `query` binding of its own; no tool, including the parent's first step, may execute |
| `conflicting-requirements.runbook.yaml` | Parent and child request incompatible versions of `scope.left`; `on_error: continue` cannot suppress preflight refusal |

The other two files in that directory are the intentionally invalid children,
not standalone success examples. To migrate the alias case, declare the child's
own `requires` and `toolRefs`, as shown in `catalog\left.runbook.yaml`.

## Verify in the graph

After installing the qualified matching extension/runtime:

1. Open an entrypoint from the table in VS Code.
2. Run **Yawr: Open Runbook Graph (React Flow)**, then **Yawr: Run Current Runbook**.
3. Follow execution into each child and its tool substitution. The child's
   `verify_left` or `verify_right` assertion must succeed.
4. After completion, navigate back through the runbook history. Earlier
   children and repeated dynamic occurrences must remain inspectable.

The static-and-lazy example must show all 14 steps and retain seven runbook
invocations, including the tool-backed runbooks inside both included children.
Version 0.2.18 fixes the qualified group-parent references that caused 0.2.17
to reject this execution graph despite successful headless execution.

For pacing checks, use the default 200 ms, then set
`yawr.preview.minimumStepDisplayMs` to 500. Ordinary CURRENT transitions must
remain a single ordered stream, including Results. Parallel execution order is
not deterministic; it must not change which definition each child invokes.

## Automated qualification

From the repository root:

```powershell
go -C runtime test ./pkg/pkgcatalog -run TestDependencyScopeExamples
go -C runtime test ./pkg/run -run TestScopedSDKExamplesRunFromCapturedEmbeddedSources
go -C runtime test ./pkg/presentation -run TestIncludedDocumentPresentation
go -C runtime test ./cmd/yawr -run TestRunStdioExecutionGraphExamples/lexical
go -C runtime test ./cmd/yawr -run TestScopedExampleRejectionsPrecedeEveryDispatch
```

The SDK regression removes every source from its embedded filesystem after
preparation, before execution. Success therefore requires captured definitions
and runbook bodies, not a late filesystem read. The presentation regression
checks each child's own tool metadata, including dirty editor overlays.
The stdio regression checks that every executed step has a graph node before
its start event, including dynamic includes inside parallel branches and tool
substitutions. It also validates every emitted group, frame, node parent, and
edge reference. The Windows scoped-loader regression resolves a captured
short-path alias outside the entrypoint directory after the source is deleted.

The installed-VSIX suite runs the unmodified static-and-lazy example through
**Yawr: Run Current Runbook**, with no helper or package-map override. Maintainers
can set `YAWR_TEST_VSCODE_VERSION=1.137.0` when running
`npm run extension:validate:vsix` to qualify the reported host; the default
remains the minimum supported VS Code 1.136.2. This is test-harness configuration,
not a setting required by runbook callers.
