// Unit tests for the pure enum-affordance/error-passthrough helpers in
// src/enumInputs.ts (compiled to out/enumInputs.js). These cover a subset
// of the client-parity acceptance matrix (AR-CE-9 §9 in
// barbara-client-enum-compatibility-ruling.md) that does not require a
// running VS Code host: CE-D-03, CE-R-01/02/03/05, CE-U-03/U-04, CE-V-05.
//
// No schema, struct, or member-validation logic is exercised or asserted
// here — these are rendering/transport decisions only (CE-C-02).
const assert = require('node:assert/strict');
const test = require('node:test');

const {
  chooseAffordance,
  extractInputDecls,
  filterRequiredInputs,
  nfcEquals,
  parseVarPairs,
  deriveFailureMessage,
  firstLine,
  warningLines,
} = require('../out/enumInputs');

test('CE-D-03: an input with no enum key gets a free-text affordance, never an empty selector', () => {
  const affordance = chooseAffordance({ name: 'region', type: 'string' });
  assert.equal(affordance.kind, 'freetext');
});

test('CE-R-01: required enum input with no default preselects nothing', () => {
  const affordance = chooseAffordance({ name: 'env', enum: ['prod', 'staging'], required: true });
  assert.equal(affordance.kind, 'selector');
  assert.deepEqual(affordance.members, ['prod', 'staging']);
  assert.equal(affordance.preselect, undefined);
  assert.equal(affordance.allowUnset, false);
});

test('CE-D-02: selector members preserve declared order verbatim, never sorted', () => {
  const affordance = chooseAffordance({ name: 'env', enum: ['staging', 'prod'] });
  assert.deepEqual(affordance.members, ['staging', 'prod']);
});

test('CE-R-02: default preselects only when it is itself a declared member', () => {
  const withMemberDefault = chooseAffordance({ name: 'env', enum: ['prod', 'staging'], default: 'staging' });
  assert.equal(withMemberDefault.preselect, 'staging');

  const withNonMemberDefault = chooseAffordance({ name: 'env', enum: ['prod', 'staging'], default: 'qa' });
  assert.equal(withNonMemberDefault.preselect, undefined, 'a non-member default must not be repaired or preselected');
});

test('CE-R-03: an optional enum input gets a distinct "leave unset" affordance flag', () => {
  const affordance = chooseAffordance({ name: 'env', enum: ['prod', 'staging'], required: false });
  assert.equal(affordance.kind, 'selector');
  assert.equal(affordance.allowUnset, true);
});

test('CE-R-04: case-only-distinct members are both retained, undeduplicated', () => {
  const affordance = chooseAffordance({ name: 'env', enum: ['Prod', 'prod'] });
  assert.equal(affordance.kind, 'selector');
  assert.deepEqual(affordance.members, ['Prod', 'prod']);
});

test('CE-R-05: enumRedacted forces a free-text affordance and never exposes members', () => {
  const affordance = chooseAffordance({
    name: 'secret_env',
    enumRedacted: true,
    enumMemberCount: 3,
    enum: ['leaked-a', 'leaked-b', 'leaked-c'], // must be ignored even if present on the wire
  });
  assert.equal(affordance.kind, 'redacted-freetext');
  assert.match(affordance.hint, /3/);
  assert.ok(!('members' in affordance), 'redacted affordance must not carry a members list');
});

test('CE-R-05: enumRedacted without a member count still yields a free-text affordance', () => {
  const affordance = chooseAffordance({ name: 'secret_env', enumRedacted: true });
  assert.equal(affordance.kind, 'redacted-freetext');
});

test('CE-U-04: nfcEquals matches an NFD candidate against an NFC member (preselect/echo comparison only)', () => {
  const nfc = '\u00e9'; // é, precomposed
  const nfd = 'e\u0301'; // e + combining acute accent
  assert.ok(nfcEquals(nfc, nfd));
  assert.ok(!nfcEquals('prod', 'staging'));
});

test('CE-U-03: nfcEquals is case-sensitive — no folding anywhere on the client', () => {
  assert.ok(!nfcEquals('PROD', 'prod'));
});

test('CE-D-03 (absence rule): no `inputs` array at all yields undefined, not []', () => {
  assert.equal(extractInputDecls({}), undefined);
  assert.equal(extractInputDecls({ inputs: null }), undefined);
  assert.equal(extractInputDecls(null), undefined);
});

test('extractInputDecls tolerates unknown sibling keys on the document (forward compatibility, AR-CE-7)', () => {
  const decls = extractInputDecls({
    schema_version: 'x',
    some_future_field: { anything: true },
    inputs: [{ name: 'env', enum: ['a', 'b'] }],
  });
  assert.equal(decls.length, 1);
  assert.equal(decls[0].name, 'env');
});

test('parseVarPairs splits on comma and first "=" only, never trims the value', () => {
  const parsed = parseVarPairs('env= prod ,region=us-east-1,broken');
  assert.deepEqual(parsed, { env: ' prod ', region: 'us-east-1' });
});

test('CE-U-02: parseVarPairs preserves leading/trailing whitespace in a value verbatim', () => {
  const parsed = parseVarPairs('name= leading-space');
  assert.equal(parsed.name, ' leading-space');
});

test('deriveFailureMessage prefers the raw stderr capture over the combined execFile message (D-3-shaped fix)', () => {
  const err = {
    message: 'Command failed: yawr dry-run --var env_name=bad fixture.runbook.yaml\nENUM-008: input "env_name" value "bad" is not a declared enum member\n',
    stderr: 'ENUM-008: input "env_name" value "bad" is not a declared enum member\n',
  };
  const msg = deriveFailureMessage(err);
  assert.equal(msg, 'ENUM-008: input "env_name" value "bad" is not a declared enum member');
  assert.ok(!msg.startsWith('Command failed'), 'coded error must not be buried under a generic "Command failed" prefix');
});

test('deriveFailureMessage falls back to err.message when no stderr was captured', () => {
  assert.equal(deriveFailureMessage(new Error('boom')), 'boom');
  assert.equal(deriveFailureMessage('boom'), 'boom');
});

test('firstLine returns the first non-empty line', () => {
  assert.equal(firstLine('\n\nENUM-008: bad\nmore detail\n'), 'ENUM-008: bad');
});

test('warningLines extracts only "yawr: warning:" lines, code intact', () => {
  const stderr = 'yawr: warning: inputs.env_name: [ENUM-W001] enum members "Prod" and "prod" are case-only-distinct\nsome other stderr noise\n';
  const lines = warningLines(stderr);
  assert.equal(lines.length, 1);
  assert.match(lines[0], /ENUM-W001/);
});

test('warningLines returns nothing for stderr with no warnings', () => {
  assert.deepEqual(warningLines('ENUM-008: bad value\n'), []);
});

// ─── filterRequiredInputs (RINP tests) ───────────────────────────────────────

test('RINP-1: filterRequiredInputs returns only inputs with required===true', () => {
  const decls = [
    { name: 'a', required: true },
    { name: 'b', required: false },
    { name: 'c' }, // required absent
    { name: 'd', required: true },
  ];
  const result = filterRequiredInputs(decls);
  assert.deepEqual(result.map(d => d.name), ['a', 'd'],
    'only inputs with required===true should be returned');
});

test('RINP-2: filterRequiredInputs returns empty array when no required inputs', () => {
  const decls = [
    { name: 'x', required: false },
    { name: 'y' },
  ];
  const result = filterRequiredInputs(decls);
  assert.deepEqual(result, []);
});

test('RINP-3: filterRequiredInputs returns all when all are required', () => {
  const decls = [
    { name: 'p', required: true },
    { name: 'q', required: true },
  ];
  const result = filterRequiredInputs(decls);
  assert.equal(result.length, 2);
});

test('RINP-4: filterRequiredInputs treats required===undefined as optional (not prompted)', () => {
  const decls = [{ name: 'opt' }]; // required absent
  assert.deepEqual(filterRequiredInputs(decls), [],
    'an input without a required field must be treated as optional');
});

test('RINP-5: filterRequiredInputs on empty array returns empty array', () => {
  assert.deepEqual(filterRequiredInputs([]), []);
});
