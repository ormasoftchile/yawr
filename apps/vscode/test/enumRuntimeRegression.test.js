// Hermetic regression tests for the extension's enum-error-derivation and
// declared-input helpers (CE-V-04/D-3, CE-W-01, CE-D-01/CE-D-02).
//
// Previously these tests invoked the real `yawr` binary at runtime, which
// made them dependent on a sibling-repo build output and caused them to
// silently skip in any CI environment where that binary was absent.  That
// is the ninth instance of the team's systemic "appears covered but is not
// reachable" bug class — see .squad/decisions/inbox/ken-skip-defect.md.
//
// **Option chosen: (a) — committed CLI-output fixtures.**
//
// Three of the four tests exercised a client-side helper function
// (deriveFailureMessage, warningLines, extractInputDecls) against live CLI
// output.  For each, the relevant CLI output has been captured once and
// committed under test/fixtures/ so the client-side logic can be verified
// without spawning a binary.  The fixtures represent the engine output at
// commit f1e5c51 / yawr binary now built from ../../../runtime on 2026-08-18.
//
// **Drift risk (stated plainly):** if the CLI changes its output shape
// (e.g., renames `inputs[]` to something else, changes the ENUM-008
// prefix, or alters ENUM-W001 formatting), these fixtures will diverge and
// the tests will continue to pass against stale data.  The live-binary
// variants (see test/integration/cli.test.js) are the defence against that
// drift; they must be run wherever `yawr` is available.
//
// **CE-S-01/CE-V-01** (a declared enum member is accepted by the CLI) was
// the fourth skipping test.  It exercises only CLI behaviour — no
// extension client function is in the loop — so (a) is not applicable:
// there is no client-side code to assert against a fixture.  That test has
// been moved to test/integration/cli.test.js where it fails (never skips)
// when YAWR_BIN is absent.  See that file for the CE-S-01/CE-V-01
// contract.
const assert = require('node:assert/strict');
const test = require('node:test');
const path = require('node:path');
const fs = require('node:fs');

const { deriveFailureMessage, firstLine, warningLines, extractInputDecls } = require('../out/enumInputs');

const FIXTURE_ERROR_STDERR = path.join(__dirname, 'fixtures', 'enum-error-stderr.txt');
const FIXTURE_WARN_STDERR  = path.join(__dirname, 'fixtures', 'enum-warn-stderr.txt');
const FIXTURE_GRAPHJSON    = path.join(__dirname, 'fixtures', 'enum-preview-graphjson.json');

// CE-V-04/D-3 regression: the ENUM-008 error that the real CLI emits must
// survive verbatim through deriveFailureMessage — it must not be buried
// under a "Command failed: ..." wrapper (D-3-shaped defect).
//
// Fixture: test/fixtures/enum-error-stderr.txt — the captured stderr from
//   `yawr dry-run --var env_name=not-a-member enum.runbook.yaml`
// at yawr binary f1e5c51 (2026-08-18).  If the CLI changes the ENUM-008
// format, refresh this fixture from the live integration suite.
test('CE-V-04/D-3 regression: ENUM-008 coded error survives verbatim through deriveFailureMessage', () => {
  const stderrText = fs.readFileSync(FIXTURE_ERROR_STDERR, 'utf8');
  // Simulate the execFile rejection error object that Node.js attaches when
  // the child exits non-zero: message carries the combined "Command failed: ..."
  // wrapper; stderr carries the raw capture.
  const err = {
    message: `Command failed: yawr dry-run --var env_name=not-a-member enum.runbook.yaml\n${stderrText}`,
    stderr: stderrText,
  };
  const message = deriveFailureMessage(err);
  // The engine prefixes enum membership errors with the input name:
  // `input "<name>": ENUM-008: <description>`.  Pin the full expected shape
  // so the assertion tests structure, not just substring presence.
  assert.match(message, /^input "[^"]+": ENUM-008:/, 'the coded error must follow the input-name prefix, not be buried in a generic wrapper');
  assert.match(firstLine(message), /ENUM-008/, 'the code must survive into the first line shown to the operator');
  assert.doesNotMatch(message, /^Command failed/, 'must never present as a bare wrapped command failure');
});

// CE-W-01 regression: the ENUM-W001 warning that the real CLI emits on
// stderr for case-only-distinct members must be surfaced by warningLines.
//
// Fixture: test/fixtures/enum-warn-stderr.txt — the captured stderr from
//   `yawr dry-run --var env_name=prod enum-warn.runbook.yaml`
// at yawr binary f1e5c51 (2026-08-18).  If the CLI changes the ENUM-W001
// format or prefix, refresh this fixture from the live integration suite.
test('CE-W-01 regression: ENUM-W001 (case-only-distinct members) is extracted by warningLines', () => {
  const stderrText = fs.readFileSync(FIXTURE_WARN_STDERR, 'utf8');
  const warnings = warningLines(stderrText);
  assert.ok(warnings.length >= 1, 'ENUM-W001 must be present in the fixture stderr');
  assert.match(warnings[0], /ENUM-W001/);
});

// CE-D-01/CE-D-02 regression: extractInputDecls must correctly parse the
// `inputs[]` array from the `yawr preview --format graphjson` document
// shape, preserving declared order verbatim (F-7, B-1/F-1/F-2).
//
// Fixture: test/fixtures/enum-preview-graphjson.json — the captured stdout
// from `yawr preview --format graphjson enum.runbook.yaml` at yawr binary
// f1e5c51 (2026-08-18), with absolute runbook paths removed (they are not
// asserted on and would make the fixture machine-specific).  If the CLI
// changes the graphjson Document shape (e.g., renames `inputs`), refresh
// this fixture from the live integration suite.
test('CE-D-01/CE-D-02 regression: extractInputDecls parses the graphjson inputs[] shape, declared order preserved', () => {
  const raw = fs.readFileSync(FIXTURE_GRAPHJSON, 'utf8');
  const doc = JSON.parse(raw);
  const decls = extractInputDecls(doc);
  assert.ok(decls, 'extractInputDecls must find the inputs[] array');
  const envDecl = decls.find((d) => d.name === 'env_name');
  assert.ok(envDecl, 'env_name declaration must be present');
  assert.deepEqual(envDecl.enum, ['prod', 'staging'], 'declared order must survive verbatim');
  assert.equal(envDecl.required, true);
  assert.ok(!envDecl.enumRedacted, 'a non-redacted declaration must not carry enumRedacted:true');
});



