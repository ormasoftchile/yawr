# Y0 baseline validation

**Result: PASS**

**Author:** Automation, Automation Security Engineer
**Execution date:** 2026-09-12
**Source commit:** `0f92b1d6c3e0e67efafb57a6df72c0af2022873d`
**Branch:** `milestone/y0-monorepo-health`

All checkout-executable required checks passed at the source commit. The native
authoring proof is an external gate and was not passed by this checkout-only
run, as recorded below.

## Environment and prerequisites

| Prerequisite | Observed value |
|---|---|
| Operating system | Microsoft Windows NT 10.0.26200.0 |
| Architecture | x64 |
| Node.js | v24.21.0 |
| npm | 11.19.0 |
| Go | go1.25.7 windows/amd64, official `go.dev` archive installed under checkout-local disposable state because Go was absent from `PATH` |
| Git | 2.55.0.windows.5 |
| VS Code Extension Host test version | 1.137.0; downloaded archive revision `07b4ff1883f94da91f6d698744fc7c3638b59720` |

The temporary Go installation was removed after validation.

## Initial state

`git rev-parse HEAD` returned the exact source commit above.
`git branch --show-current` returned `milestone/y0-monorepo-health`.
`git status --short --branch` returned only the branch header, with no changes.

The known disposable paths were absent before execution:

- `apps\vscode\out`
- `apps\vscode\.vscode-test`
- `apps\vscode\yawr-preview.vsix`
- `.y0-candidate`
- `.y0-alt-path`

They were nevertheless removed with narrow, explicit path operations before
the gates, and their absence was rechecked. No source directory or broad
repository directory was removed.

Initial SHA-256 values:

| Tracked asset | SHA-256 |
|---|---|
| `package-lock.json` | `537e20dab7e7ebcd7acbf069ad28249bff39d5d3732642c6ec9a0897307a2ac1` |
| `runtime\go.sum` | `b1739cca03f862b81a44fd65da2bc75e53ecf7dc57dcb9f03298629380bb07df` |
| `apps\vscode\media\graph.css` | `4737cd1c3deca826b37738460770d2a201cfe7f35c2a520a4f88cde6106429a3` |
| `apps\vscode\media\graph.js` | `2bac83bff903b647b2d7c75dcbb2412fe347b9a994e195366c65b6cdb11d7c16` |
| `apps\vscode\media\highlighting-worker.js` | `3f479cb2fc8cd19806b7caa8868a4dd208c70861b45063438b77f704652a894f` |
| `apps\vscode\media\highlighting-NOTICES.txt` | `615999cd22689b40ee97b78f5713af0b4ca9778d46a8c888468343cf3f59f265` |
| `apps\vscode\bin\win32-x64\yawr.exe` | `61419fdd4826c784c2493a8eacac656197b7baddb50be1be5279971b5e26a56d` |

## Command results

Commands ran from the repository root unless the command itself selected a
component with `-C` or an npm workspace.

| Command | Exit | Duration | Important raw-result summary |
|---|---:|---:|---|
| `npm ci` | 0 | 38.580 s | Added 464 packages; audited 466. npm reported 14 dependency vulnerabilities: 2 low, 4 moderate, 8 high. |
| `npm run check:root` | 0 | 0.791 s | `Verified Yawr-only root wiring.` |
| `npm run runtime:build` | 0 | 12.167 s | `go build -C runtime -trimpath -buildvcs=false ./cmd/yawr` completed. |
| `npm run runtime:test` | 0 | 85.747 s | `go test -C runtime ./... -count=1`; all reported packages passed or had no test files. Longest reported package included `runtime\cmd\yawr` at 71.192 s. |
| `go test -C runtime ./internal/engine -run '^TestRouteTest_ParallelBoundaryDominatesJoinRecovery$' -count=50` | 0 | 5.470 s | Named engine regression passed 50 uncached repetitions; package result `ok`, 0.428 s reported by Go. |
| `npm run extension:compile` | 0 | 16.118 s | TypeScript extension sources compiled; React graph bundle and highlighting worker built. |
| `npm run extension:test` | 0 | unavailable after console capture truncation | The lifecycle unit gate compiled and completed successfully. Its cleanup removed `apps\vscode\out`, demonstrating later gates could not consume stale compiled output. |
| `npm run extension:test:relocation` | 0 | 1.505 s | 9 tests, 9 passed, 0 failed, 0 skipped; source-host configuration, monorepo component wiring, and repository-boundary rules passed. |
| `go build -C runtime -trimpath -buildvcs=false -o '..\.y0-candidate\yawr.exe' ./cmd/yawr` | 0 | 3.227 s | Built a disposable helper from this checkout: 29,882,368 bytes, SHA-256 `b3fe55576d6a5ab94c9a42bece9d1d82f570339812d7e9331763d3e44443908a`. |
| `npm run extension:package-helper -- C:\One\Opensource\yawr\.y0-candidate\yawr.exe` | 1 | 1.466 s | First attempt correctly failed because the preceding lifecycle unit gate had removed compiled `out` state: `Cannot find module '../out/presentationProtocol.js'`. No production file was edited. |
| `npm run extension:compile` | 0 | 12.219 s | Recovery recompiled the required output after lifecycle cleanup. |
| `npm run extension:package-helper -- C:\One\Opensource\yawr\.y0-candidate\yawr.exe` | 0 | 4.046 s | `Verified current authoring v3 parity`; candidate and packaged helper SHA-256 were both `b3fe55576d6a5ab94c9a42bece9d1d82f570339812d7e9331763d3e44443908a`. |
| `npm run extension:verify-authoring-parity -- C:\One\Opensource\yawr\.y0-candidate\yawr.exe` | 0 | 1.655 s | `Source and packaged authoring v3 behavior match; obsolete invocation is rejected.` |
| `npm run extension:package-helper -- C:\One\Opensource\yawr\.y0-candidate\nonexistent-yawr.exe` with `.y0-alt-path` first on `PATH` | 1 as required | 1.480 s | Failed with `ENOENT` for the exact explicit path while `.y0-alt-path\yawr.exe` existed. The wrapper converted the expected nonzero result into a passing negative control. |
| `npm run extension:package:validate` | 0 | 45.138 s | Matching packaged helper capabilities verified. Disposable VSIX contained 266 files and was 15.44 MB. |
| `npm run extension:e2e` | 0 | 65.905 s | VS Code 1.137.0 source Extension Host ran 24 tests: 24 passing, 0 skipped, 0 failed. |
| `npm run extension:validate:vsix` | 0 | 75.879 s | VSIX packaged, installed successfully, and the installed production-surface test passed: 1 passing, followed by `extension:validate:vsix PASS installed-vsix`. |

The initial explicit packaging failure was a non-vacuity observation and was
recovered within the allowed cycles by rerunning the documented compile
prerequisite. The required final packaging and parity executions passed.

## Extension Host execution evidence

The source-host run was not a wiring-only check. VS Code 1.137.0 launched a
local Extension Host, loaded the development extension from this checkout, and
reported **24 discovered, 24 passing, 0 skipped, 0 failed**. Executed surfaces
included activation and command registration, the bundled graph webview,
refresh/race behavior, direct stdio runs, choice and external view flows, saved route
tests, debug overrides, preview placement, host-action round trips, and the
investigation-session graph.

The installed-VSIX run was also an actual Extension Host execution:

- the CLI reported `Extension 'yawr-preview.vsix' was successfully installed`;
- the host loaded only the disposable VSIX test harness as a development
  extension;
- the test resolved `ormasoftchile.yawr-preview` from the isolated installed
  extensions directory, asserted package name `yawr-preview`, and activated it;
- it asserted that `yawr.previewGraph` was registered and that removed or
  test-only commands were absent;
- it verified the packaged Windows x64 helper exists;
- it executed `yawr.previewGraph` against an installed fixture;
- it asserted the opened tab was a webview with canonical view type
  `yawrPreviewGraph`;
- the host reported **1 discovered, 1 passing, 0 skipped, 0 failed** and the
  lifecycle emitted `PASS installed-vsix`.

## Explicit candidate and non-vacuity controls

The helper used for positive packaging was built from the frozen checkout and
passed as an explicit absolute candidate argument. Before packaging it was
29,882,368 bytes with SHA-256
`b3fe55576d6a5ab94c9a42bece9d1d82f570339812d7e9331763d3e44443908a`.
The packaged helper had the identical hash. Authoring v3 parity passed and the
obsolete invocation was rejected.

For the negative control, a valid alternate helper was present first on
`PATH`, while the explicit argument named a nonexistent candidate. Packaging
failed with `ENOENT` naming that explicit candidate. This proves the positive
result did not silently select an alternate `PATH` helper.

## Native authoring external gate

**EXTERNALLY GATED — NOT RUN — NOT PASSED**

No authoritative `native-config.json` evidence bundle exists inside Yawr.
The missing prerequisite is the prepared external bundle containing the real
helper, YAML extension, VS Code executable, caller runbook, caller project
root, and caller package map. CI wiring or checkout-only tests were not used as
a substitute.

## Cleanup and final integrity

Validation lifecycle cleanup removed its `out`, `.vscode-test`, and disposable
VSIX state. Manual cleanup removed the checkout-local Go archive and
installation, both helper candidate directories, npm installation state, and
the runtime build output. The tracked packaged helper was returned to its
source-commit bytes without changing repository history.

Final checks confirmed:

- `apps\vscode\out`, `apps\vscode\.vscode-test`,
  `apps\vscode\yawr-preview.vsix`, `.y0-candidate`, `.y0-alt-path`,
  `.tools`, `node_modules`, and `runtime\yawr.exe` are absent;
- every tracked hash listed in **Initial state** is unchanged;
- `git rev-parse HEAD` remains the exact source commit;
- the final tracked worktree contains only this report.

## Residual untested surfaces

- Native authoring behavior requiring the external evidence bundle was not
  run and is not passed.
- This run proves Windows x64 locally; it does not claim Linux, macOS, ARM64,
  Marketplace publication, upgrade/coexistence, or other VS Code versions.
- Network-isolated or enterprise-managed VS Code environments were not tested.
- `npm ci` reported dependency vulnerabilities, but this closeout rerun did
  not perform a dependency audit, remediation, or exploitability assessment.
- No release, publication, signing, or distribution operation was performed.

## Independent Review

**Reviewer:** Tester, Conformance Tester
**Review timestamp:** `2026-09-12T07:58:19.1842525-07:00`
**Verdict:** **PASS**

I independently reviewed the report, its only-file worktree change, the
validation scripts, and the production-surface assertions. The reviewed HEAD
was `0f92b1d6c3e0e67efafb57a6df72c0af2022873d` on
`milestone/y0-monorepo-health`. The review host was Microsoft Windows NT
10.0.26200.0 x64 with Node.js v24.21.0, npm 11.19.0, Go 1.25.7 windows/amd64,
and Git 2.55.0.windows.5.

The rerun began with `apps\vscode\out`, `apps\vscode\.vscode-test`,
`apps\vscode\yawr-preview.vsix`, `.y0-candidate`, `.y0-alt-path`, `.tools`,
`node_modules`, and `runtime\yawr.exe` absent. `go` was also absent from
`PATH`; the review provisioned Go 1.25.7 only under disposable checkout-local
`.tools` state. Commands below ran from the repository root; `<repo>` denotes
that root.

| Independently executed command | Exit | Result |
|---|---:|---|
| `npm ci` | 0 | 464 packages installed; 466 audited; the same 14 vulnerability notices were reported. |
| `npm run check:root` | 0 | `Verified Yawr-only root wiring.` |
| `npm run runtime:build` | 0 | Runtime built from this checkout. |
| `npm run runtime:test` | 0 | Uncached `go test -C runtime ./... -count=1` completed with all package results passing or reporting no test files. |
| `go test -C runtime ./internal/engine -run '^TestRouteTest_ParallelBoundaryDominatesJoinRecovery$' -count=50` | 0 | Named engine regression passed all 50 repetitions. |
| `npm run extension:test` | 0 | A capture rerun reported 561 tests discovered: 561 passed, 0 failed, 0 skipped, 0 todo. The lifecycle removed `out` after each run. |
| `npm run extension:compile` | 0 | Recreated extension, webview, and highlighting output after lifecycle cleanup. |
| `npm run extension:test:relocation` | 0 | 9 discovered: 9 passed, 0 failed, 0 skipped. |
| `go build -C runtime -trimpath -buildvcs=false -o '..\.y0-candidate\yawr.exe' ./cmd/yawr` | 0 | Candidate was 29,882,368 bytes, SHA-256 `b3fe55576d6a5ab94c9a42bece9d1d82f570339812d7e9331763d3e44443908a`. |
| `npm run extension:package-helper -- <repo>\.y0-candidate\yawr.exe` | 0 | Current authoring v3 parity verified; packaged helper hash exactly matched the candidate. |
| `npm run extension:verify-authoring-parity -- <repo>\.y0-candidate\yawr.exe` | 0 | Source/packaged behavior matched and the obsolete invocation was rejected. |
| `npm run extension:package-helper -- <repo>\.y0-candidate\nonexistent-yawr.exe` with `<repo>\.y0-alt-path` first on `PATH` | 1, required | Failed with `ENOENT` naming the explicit nonexistent candidate even though a valid alternate `yawr.exe` was on `PATH`; the negative-control wrapper exited 0. |
| `npm run extension:package:validate` | 0 | Matching packaged capabilities verified; VSIX contained 266 files and was 15.44 MB. |
| `npm run extension:e2e` | 0 | A local source Extension Host executed 24 discovered tests: 24 passing, 0 skipped, 0 failed. |
| `npm run extension:validate:vsix` | 0 | VSIX installation succeeded; installed production surface executed 1 discovered test: 1 passing, 0 skipped, 0 failed; lifecycle printed `extension:validate:vsix PASS installed-vsix`. |

The installed test is non-vacuous: it resolves
`ormasoftchile.yawr-preview` from the isolated installed extensions directory,
checks package name `yawr-preview`, activates the extension, verifies
`yawr.previewGraph`, executes it against the installed fixture, and requires a
webview tab whose canonical view type is `yawrPreviewGraph`. The run loaded
only `apps\vscode\test\vsix-harness` as a development extension and reported
successful VSIX installation before that assertion passed.

The report diff contains only `docs\y0-baseline.md`. I found no credential,
raw environment dump, user-profile path, substituted CI-wiring claim, or
unsupported pass claim. Initial and restored hashes matched for
`package-lock.json`, `runtime\go.sum`, all four tracked generated media assets,
and `apps\vscode\bin\win32-x64\yawr.exe`.

No `native-config.json` exists in Yawr. The authoritative native runner
requires absolute existing paths for the real helper, YAML extension, VS Code
executable, caller runbook, caller project root, and caller package map.
Therefore native authoring remains **EXTERNALLY GATED — NOT RUN — NOT
PASSED**; no checkout or CI-wiring evidence was accepted as a substitute.

Residuals are unchanged: native authoring was not executed; this proves only
Windows x64 and VS Code 1.137.0 locally; other operating systems,
architectures, VS Code versions, managed/offline environments, publication,
signing, upgrade/coexistence, and dependency-vulnerability remediation remain
untested.
