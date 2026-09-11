// repoBoundary.test.js — repo-wide guard against cross-repo path leaks,
// unconditional skips, and orphaned test files.
//
// Mirrors yawr/internal/tool/repo_boundary_test.go
// (TestRepoBoundary_NoExternalPaths), which Don added in response to the
// same systemic defect class.
//
// **Rules enforced (one top-level test per rule — failures name the rule):**
//
//  1. Sibling-repo name references — any test/src file that contains a
//     sibling repo name (assembled at runtime so this guard doesn't
//     trigger itself).
//
//  2. Cross-repo path traversals — `path.join(__dirname, '..', '..', 'yawr'`
//     and `'../yawr'`-style literals in test/src files.
//
//  3. `process.env.YAWR_BIN` in unit test files — the canonical escape
//     hatch to a sibling-repo binary; prohibited in test/*.test.js.
//
//  4. `skip:` in test declarations — the { skip: ... } option passed to
//     `test(name, opts, fn)`; a skipping test is the defect pattern
//     described in .squad/decisions/inbox/ken-skip-defect.md.
//
//  5. Orphaned test files — every test/**/*.test.js file must be reachable
//     by at least one glob in a `node --test <glob>` npm script.  A file
//     that no script can reach is invisible to every runner — strictly
//     worse than a skipped test (a skip is at least reported).
//
// **Allowlists:** every entry MUST carry a justifying comment.
// An empty allowlist is the correct starting state.

'use strict';

const assert = require('node:assert/strict');
const test   = require('node:test');
const path   = require('node:path');
const fs     = require('node:fs');
const yaml   = require('js-yaml');
const ts     = require('typescript');

const REPO_ROOT = path.join(__dirname, '..');
const TEST_DIR  = __dirname;
const SRC_DIR   = path.join(REPO_ROOT, 'src');

// ── Sibling-repo patterns (assembled at runtime; guard file safe) ─────────
const SIBLING_REPO_PATTERNS = [
  'yawr' + '-private',  // private companion repo — never read from tests
];

// ── Cross-repo path literals to ban ──────────────────────────────────────
const CROSS_REPO_PATH_LITERALS = [
  "path.join(__dirname, '..', '..', 'yawr'",
  'path.join(__dirname, "..", "..", "yawr"',
  "'../yawr",
  '"../yawr',
  '`../yawr',
];

// ── Skip allowlist ────────────────────────────────────────────────────────
// Files permitted to contain `skip:` — must have a justifying comment.
// An unjustified entry or a blanket allowlist is not acceptable.
const SKIP_ALLOWLIST = {
  // No entries — there are no justified uses of skip: in unit test files.
  // Tests that require an unavailable precondition must FAIL with an
  // actionable message, never skip silently.
};

// ── Orphan allowlist ──────────────────────────────────────────────────────
// Test files NOT reachable by any configured runner, with a mandatory
// justification per entry.  This allowlist should be empty; entries here
// represent accepted blind spots that must be re-evaluated on each review.
const ORPHAN_ALLOWLIST = {
  // No entries — every test/**/*.test.js must be reachable by a script.
};

// ── Helpers ───────────────────────────────────────────────────────────────

/** Collect all *.test.js files under a directory, recursively. */
function collectTestJs(dir, relPrefix) {
  const out = [];
  if (!fs.existsSync(dir)) return out;
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const rel = relPrefix ? relPrefix + '/' + e.name : e.name;
    if (e.isDirectory()) {
      out.push(...collectTestJs(path.join(dir, e.name), rel));
    } else if (e.name.endsWith('.test.js')) {
      out.push({ abs: path.join(dir, e.name), rel });
    }
  }
  return out;
}

/** Collect all .ts and .js source files under a directory, recursively. */
function collectSrc(dir, relPrefix) {
  const out = [];
  if (!fs.existsSync(dir)) return out;
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const rel = relPrefix ? relPrefix + '/' + e.name : e.name;
    if (e.isDirectory()) {
      out.push(...collectSrc(path.join(dir, e.name), rel));
    } else if (e.name.endsWith('.ts') || e.name.endsWith('.js')) {
      out.push({ abs: path.join(dir, e.name), rel });
    }
  }
  return out;
}

/**
 * Very small glob matcher supporting `*` (one segment) and `**` (any depth).
 * Both pattern and filepath use forward slashes.
 * Used to check whether a file is reachable by a `node --test <glob>` script.
 */
function globMatch(pattern, filepath) {
  const pParts = pattern.split('/');
  const fParts = filepath.split('/');
  function match(pi, fi) {
    if (pi === pParts.length && fi === fParts.length) return true;
    if (pi === pParts.length) return false;
    if (pParts[pi] === '**') {
      // ** matches zero or more segments
      for (let k = fi; k <= fParts.length; k++) {
        if (match(pi + 1, k)) return true;
      }
      return false;
    }
    if (fi === fParts.length) return false;
    const seg = pParts[pi];
    const fSeg = fParts[fi];
    // Convert glob segment to regex: * → [^/]*, . → \.
    const re = new RegExp('^' + seg.replace(/\./g, '\\.').replace(/\*/g, '[^/]*') + '$');
    return re.test(fSeg) && match(pi + 1, fi + 1);
  }
  return match(0, 0);
}

/**
 * Extract `node --test <glob>` glob patterns from all npm scripts in
 * package.json.  Returns an array of glob strings (forward-slash paths).
 * This is the set of globs that `npm test` (and any test:* script) uses
 * to discover test files.
 */
function extractTestGlobs() {
  const pkg = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, 'package.json'), 'utf8'));
  const scripts = pkg.scripts || {};
  const globs = [];
  for (const cmd of Object.values(scripts)) {
    if (typeof cmd !== 'string') continue;
    // Match `node --test [flags...] <glob>` — glob may be bare, single-quoted, or
    // double-quoted, and is terminated by whitespace or end of string.
    // Flags (e.g. --test-timeout=5000) are optional between --test and the glob.
    const re = /node\s+--test(?:\s+--[\w][\w=-]*)*\s+(['"]?)([^\s'"]+)\1/g;
    let m;
    while ((m = re.exec(cmd)) !== null) {
      globs.push(m[2].replace(/\\/g, '/'));
    }
    const lifecycle = /extension-lifecycle\.mjs\s+unit\s+(['"]?)([^\s'"]+)\1/g;
    while ((m = lifecycle.exec(cmd)) !== null) {
      globs.push(m[2].replace(/\\/g, '/'));
    }
  }
  return globs;
}

function findSkippedTestDeclarations(content, filename) {
  const source = ts.createSourceFile(filename, content, ts.ScriptTarget.Latest, true, ts.ScriptKind.JS);
  const findings = [];
  const testNames = new Set(['test', 'it', 'describe', 'suite']);

  function propertyName(node) {
    if (!node) return undefined;
    if (ts.isIdentifier(node) || ts.isStringLiteral(node) || ts.isNumericLiteral(node)) return node.text;
    if (ts.isComputedPropertyName(node) && ts.isStringLiteral(node.expression)) return node.expression.text;
    return undefined;
  }

  function isTestCallee(expression) {
    return ts.isIdentifier(expression) && testNames.has(expression.text);
  }

  function isEnabledSkipProperty(property) {
    if (propertyName(property.name) !== 'skip') return false;
    if (ts.isPropertyAssignment(property)) {
      const value = property.initializer;
      return value.kind !== ts.SyntaxKind.FalseKeyword
        && value.kind !== ts.SyntaxKind.NullKeyword
        && !(ts.isIdentifier(value) && value.text === 'undefined');
    }
    return ts.isShorthandPropertyAssignment(property) || ts.isMethodDeclaration(property)
      || ts.isGetAccessorDeclaration(property) || ts.isSetAccessorDeclaration(property);
  }

  function visit(node) {
    if (ts.isCallExpression(node)) {
      const callee = node.expression;
      if (
        ts.isPropertyAccessExpression(callee)
        && callee.name.text === 'skip'
        && isTestCallee(callee.expression)
      ) {
        findings.push(source.getLineAndCharacterOfPosition(callee.name.getStart(source)).line + 1);
      } else if (isTestCallee(callee)) {
        for (const argument of node.arguments) {
          if (
            ts.isObjectLiteralExpression(argument)
            && argument.properties.some(isEnabledSkipProperty)
          ) {
            findings.push(source.getLineAndCharacterOfPosition(argument.getStart(source)).line + 1);
          }
        }
      }
    }
    ts.forEachChild(node, visit);
  }

  visit(source);
  return findings;
}

// ── Tests (one per rule) ──────────────────────────────────────────────────

const testFiles = collectTestJs(TEST_DIR, '');
const srcFiles  = collectSrc(SRC_DIR, 'src');
const allFiles  = [...testFiles, ...srcFiles];

// Rule 1 — sibling-repo name references
test('repoBoundary/rule1: no sibling-repo name references in test/ or src/', () => {
  const violations = [];
  for (const { abs, rel } of allFiles) {
    if (path.basename(abs) === 'repoBoundary.test.js') continue;
    const lower = fs.readFileSync(abs, 'utf8').toLowerCase();
    for (const pat of SIBLING_REPO_PATTERNS) {
      if (lower.includes(pat.toLowerCase())) {
        violations.push(
          `${rel}: contains cross-repo reference ${JSON.stringify(pat)} — ` +
          `tests must read only files inside the repo root; ` +
          `move the fixture into test/fixtures/ and delete the external path`
        );
      }
    }
  }
  if (violations.length > 0) {
    assert.fail('rule1 violations:\n' + violations.map((v, i) => `  [${i+1}] ${v}`).join('\n'));
  }
});

// Rule 2 — cross-repo path traversals
test('repoBoundary/rule2: no cross-repo path traversals in test/ or src/', () => {
  const violations = [];
  for (const { abs, rel } of allFiles) {
    if (path.basename(abs) === 'repoBoundary.test.js') continue;
    const content = fs.readFileSync(abs, 'utf8');
    for (const banned of CROSS_REPO_PATH_LITERALS) {
      if (content.includes(banned)) {
        violations.push(
          `${rel}: contains cross-repo path ${JSON.stringify(banned)} — ` +
          `this escapes to the sibling engine repo; use test/fixtures/ instead`
        );
      }
    }
  }
  if (violations.length > 0) {
    assert.fail('rule2 violations:\n' + violations.map((v, i) => `  [${i+1}] ${v}`).join('\n'));
  }
});

// Rule 3 — process.env.YAWR_BIN in unit test files
test('repoBoundary/rule3: no process.env.YAWR_BIN in unit test files (test/*.test.js)', () => {
  const violations = [];
  for (const { abs, rel } of testFiles) {
    if (path.basename(abs) === 'repoBoundary.test.js') continue;
    // Only check files directly under test/ (depth 1 — the unit test layer).
    // Subdirectories (test/integration/, test/suite/) are out of scope for
    // this rule because they may legitimately require a binary.
    if (rel.includes('/')) continue;
    const content = fs.readFileSync(abs, 'utf8');
    if (content.includes('process.env.YAWR_BIN')) {
      violations.push(
        `${rel}: references process.env.YAWR_BIN — this is a sibling-repo binary ` +
        `escape that makes the test non-hermetic. Unit test files must not require ` +
        `an external binary. If this test exercises only CLI behaviour with no ` +
        `extension code in the loop, it cannot be made hermetic from this repo; ` +
        `delete it and guard the contract in the yawr test suite instead.`
      );
    }
  }
  if (violations.length > 0) {
    assert.fail('rule3 violations:\n' + violations.map((v, i) => `  [${i+1}] ${v}`).join('\n'));
  }
});

// Rule 4 — skip: in test declarations
test('repoBoundary/rule4: no skip: in test declarations in test/*.test.js', () => {
  assert.deepEqual(
    findSkippedTestDeclarations("test('mutant', { skip: true }, () => {});", 'mutation.js'),
    [1],
    'mutation proof: an enabled test option must be rejected'
  );
  assert.deepEqual(
    findSkippedTestDeclarations("test.skip('mutant', () => {});", 'mutation.js'),
    [1],
    'mutation proof: test.skip declarations must be rejected'
  );
  assert.deepEqual(
    findSkippedTestDeclarations("const runtime = { skip: { status: 'skipped' } };", 'control.js'),
    [],
    'non-test object keys named skip must not be rejected'
  );
  assert.deepEqual(
    findSkippedTestDeclarations("test('control', { skip: false }, () => {});", 'control.js'),
    [],
    'disabled skip options must not be rejected'
  );

  const violations = [];
  for (const { abs, rel } of testFiles) {
    if (path.basename(abs) === 'repoBoundary.test.js') continue;
    // Only check files directly under test/ — same scope as rule 3.
    if (rel.includes('/')) continue;
    const content = fs.readFileSync(abs, 'utf8');
    const skippedLines = findSkippedTestDeclarations(content, abs);
    if (skippedLines.length > 0) {
      const reason = SKIP_ALLOWLIST[rel];
      if (!reason) {
        violations.push(
          `${rel}:${skippedLines.join(',')}: contains a skipped test declaration or enabled skip option — ` +
          `a skip must never be the default outcome (ken-skip-defect.md). ` +
          `Tests that require an unavailable precondition must FAIL with an ` +
          `actionable message. ` +
          `If a justified exception is truly needed, add the file to SKIP_ALLOWLIST ` +
          `in test/repoBoundary.test.js with a comment explaining why.`
        );
      }
    }
  }
  if (violations.length > 0) {
    assert.fail('rule4 violations:\n' + violations.map((v, i) => `  [${i+1}] ${v}`).join('\n'));
  }
});

// Rule 5 — orphaned test files
//
// Every test/**/*.test.js must be reachable by at least one glob in a
// `node --test <glob>` npm script.  An unreachable file is an orphan: it
// appears in the directory listing and looks like coverage but provides
// none — strictly worse than a skipped test (a skip is at least reported).
//
// Implementation: reads package.json at runtime so this check stays correct
// when scripts are updated without touching this file.
test('repoBoundary/rule5: every test/**/*.test.js is reachable by a configured test runner', () => {
  const testGlobs = extractTestGlobs();
  assert.ok(
    testGlobs.length > 0,
    'repoBoundary: no `node --test <glob>` patterns found in package.json scripts — ' +
    'the orphan check cannot run without at least one configured pattern'
  );

  const violations = [];
  for (const { rel } of testFiles) {
    if (path.basename(rel.split('/').pop()) === 'repoBoundary.test.js') continue;

    // Normalise the relative path (from repo root) to forward slashes.
    // The file is under test/, so its repo-root-relative path is 'test/' + rel.
    const relFromRoot = 'test/' + rel.replace(/\\/g, '/');

    const reachable = testGlobs.some(g => globMatch(g, relFromRoot));
    if (!reachable) {
      const reason = ORPHAN_ALLOWLIST[rel];
      if (!reason) {
        violations.push(
          `${rel}: this test file is not reachable by any configured test runner. ` +
          `Configured globs: ${testGlobs.map(g => JSON.stringify(g)).join(', ')}. ` +
          `Either wire this file into a script in package.json AND add a CI job ` +
          `that invokes that script (rule6 enforces the CI wiring), ` +
          `or delete it — an orphan provides no coverage and misleads readers. ` +
          `If a justified exception is needed, add the file to ORPHAN_ALLOWLIST in ` +
          `test/repoBoundary.test.js with a comment explaining why.`
        );
      } else {
        console.log(`repoBoundary/rule5: orphan allowed for ${rel} (${reason})`);
      }
    }
  }
  if (violations.length > 0) {
    assert.fail('rule5 (orphan) violations:\n' + violations.map((v, i) => `  [${i+1}] ${v}`).join('\n'));
  }
});

// Rule 7 — every getConfiguration( call in src/ must be resource-scoped or
// carry an explicit window-scoped justification marker.
//
// A call to vscode.workspace.getConfiguration without a second (resource) URI
// silently falls back to workspaceFolders[0], which is the wrong folder in
// any multi-root workspace where the active runbook belongs to a different
// folder.  Every call site must either:
//
//   (a) pass a second argument  — detected by a comma after getConfiguration(
//       on the same line or the immediately following line, or
//   (b) carry the marker `// window-scoped:` on the same line or the
//       immediately preceding line, with a reason following the colon.
//
// JUSTIFICATION MARKER FORMAT: `// window-scoped: <reason>`
// Applying the marker is an explicit, reviewable decision recorded in source.
// Do NOT add a blanket allowlist here — the marker belongs at the call site.
test('repoBoundary/rule7: every getConfiguration( in src/ is resource-scoped or window-scoped:', () => {
  // Collect all .ts files under src/ from disk so a new file auto-trips the rule.
  function collectTs(dir) {
    const out = [];
    if (!fs.existsSync(dir)) return out;
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (e.isDirectory()) {
        out.push(...collectTs(path.join(dir, e.name)));
      } else if (e.name.endsWith('.ts')) {
        out.push(path.join(dir, e.name));
      }
    }
    return out;
  }

  const MARKER = '// window-scoped:';
  const violations = [];
  let totalCallSites = 0;

  for (const abs of collectTs(SRC_DIR)) {
    const rel = path.relative(REPO_ROOT, abs).replace(/\\/g, '/');
    const lines = fs.readFileSync(abs, 'utf8').split('\n');

    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];

      // Skip this guard file itself.
      if (path.basename(abs) === 'repoBoundary.test.js') continue;

      // Only care about lines that contain a getConfiguration( call (not in a
      // comment or string — we check by requiring it appears outside a leading //)
      const trimmed = line.trimStart();
      if (!line.includes('getConfiguration(')) continue;
      // Skip comment-only lines (the call appears only in a comment).
      if (trimmed.startsWith('//') || trimmed.startsWith('*')) continue;

      totalCallSites++;

      // (a) Does this line or the next line contain a comma after getConfiguration(?
      // We look from the first getConfiguration( occurrence onward on this line,
      // and if the line has no closing ) before the comma we also check next line.
      const callIdx = line.indexOf('getConfiguration(');
      const afterCall = line.slice(callIdx + 'getConfiguration('.length);
      // Find the first comma not nested inside inner parens.
      function hasTopLevelComma(text) {
        let depth = 0;
        for (const ch of text) {
          if (ch === '(') { depth++; continue; }
          if (ch === ')') { if (depth === 0) return false; depth--; continue; }
          if (ch === ',' && depth === 0) return true;
        }
        return false;
      }
      const hasCommaOnSameLine = hasTopLevelComma(afterCall);
      const hasCommaOnNextLine = !hasCommaOnSameLine &&
        i + 1 < lines.length && hasTopLevelComma(lines[i + 1]);

      if (hasCommaOnSameLine || hasCommaOnNextLine) continue; // resource-scoped ✓

      // (b) Does this line or any immediately preceding contiguous comment line
      //     carry // window-scoped:?  We walk backwards through comment-only lines.
      let markerFound = line.includes(MARKER);
      if (!markerFound) {
        for (let j = i - 1; j >= 0 && !markerFound; j--) {
          const pLine = lines[j].trimStart();
          if (pLine.startsWith('//') || pLine.startsWith('*')) {
            if (lines[j].includes(MARKER)) { markerFound = true; }
          } else {
            break; // non-comment line — stop looking
          }
        }
      }
      if (markerFound) continue; // justified ✓

      violations.push(
        `${rel}:${i + 1}: unscoped getConfiguration( without a resource argument or ` +
        `'${MARKER}' justification — either pass a second resource URI argument to ` +
        `scope the lookup to the correct workspace folder, or add ` +
        `'${MARKER} <reason>' on the preceding line if window-scope is genuinely correct. ` +
        `Offending line: ${line.trim()}`
      );
    }
  }

  assert.ok(
    totalCallSites > 0,
    'repoBoundary/rule7: inspected 0 getConfiguration( call sites in src/ — ' +
    'the rule is not running correctly; check that SRC_DIR resolves to the src/ directory'
  );

  if (violations.length > 0) {
    assert.fail(
      `rule7 violations (${violations.length} unscoped getConfiguration call site(s)):\n` +
      violations.map((v, i) => `  [${i + 1}] ${v}`).join('\n')
    );
  }
});

// Rule 6 — unwired retained product gates
//
// Rule5 proves a test file is reachable by *a configured npm script*.
// Rule6 closes the remaining monorepo links: every retained component test
// runner must have an explicit root package.json wrapper, and every wrapper
// must be invoked by a root CI workflow:
//
//   test file → matched by a script glob (rule5)
//             → component runner → root wrapper → root CI (rule6)
//
// test:e2e:only is intentionally not a product gate: it is the low-level
// unlabelled vscode-test utility used for interactive selection. The labelled
// source and installed-VSIX runners are the retained CI gates.
test('repoBoundary/rule6: every retained product test runner is invoked by root CI', () => {
  const monorepoRoot = path.resolve(REPO_ROOT, '..', '..');
  const componentPkg = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, 'package.json'), 'utf8'));
  const rootPkg = JSON.parse(fs.readFileSync(path.join(monorepoRoot, 'package.json'), 'utf8'));
  const workflowDir = path.join(monorepoRoot, '.github', 'workflows');
  const workflowFiles = fs.existsSync(workflowDir)
    ? fs.readdirSync(workflowDir)
      .filter((name) => name.endsWith('.yml') || name.endsWith('.yaml'))
    : [];
  const workflowCommands = [];
  for (const name of workflowFiles) {
    const workflow = yaml.load(fs.readFileSync(path.join(workflowDir, name), 'utf8'));
    for (const job of Object.values(workflow?.jobs || {})) {
      for (const step of job?.steps || []) {
        if (typeof step.run === 'string') workflowCommands.push(step.run);
      }
    }
  }
  const combinedWorkflowCommands = workflowCommands.join('\n');

  const nodeTestScripts = Object.entries(componentPkg.scripts || {})
    .filter(([, cmd]) => typeof cmd === 'string' && (
      /node\s+--test/.test(cmd) || /extension-lifecycle\.mjs\s+unit/.test(cmd)
    ))
    .map(([name]) => name);
  const retainedProductGates = [...new Set([
    ...nodeTestScripts,
    'test:authoring:native',
    'test:e2e',
    'test:e2e:vsix',
  ])];
  const rootGateNames = {
    'test:e2e': 'extension:e2e',
    'test:e2e:vsix': 'extension:e2e:vsix',
  };

  assert.ok(
    nodeTestScripts.length > 0,
    'repoBoundary/rule6: no npm scripts containing `node --test` found — ' +
    'this guard requires at least one test script to be meaningful'
  );

  const violations = [];
  for (const name of retainedProductGates) {
    assert.equal(
      typeof componentPkg.scripts?.[name],
      'string',
      `repoBoundary/rule6: retained component test runner "${name}" is missing`
    );

    const rootName = rootGateNames[name] || (name === 'test' ? 'extension:test' : `extension:${name}`);
    const rootCommand = rootPkg.scripts?.[rootName];
    const expectedWorkspaceInvocation = name === 'test'
      ? /\bnpm\s+(?:test|run\s+test)(?!\S)[^]*--workspace\s+apps\/vscode(?!\S)/
      : new RegExp(
        `\\bnpm\\s+run\\s+${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?!\\S)[^]*` +
        '--workspace\\s+apps/vscode(?!\\S)'
      );

    if (typeof rootCommand !== 'string' || !expectedWorkspaceInvocation.test(rootCommand)) {
      violations.push(
        `component script "${name}" is not exposed by root package.json script "${rootName}" ` +
        `as an explicit apps/vscode workspace invocation`
      );
      continue;
    }

    const escapedRootName = rootName.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    if (!new RegExp(`\\bnpm\\s+run\\s+${escapedRootName}(?!\\S)`).test(combinedWorkflowCommands)) {
      violations.push(
        `root script "${rootName}" for retained component runner "${name}" is not invoked ` +
        'by any .github/workflows/*.yml step'
      );
    }
  }
  if (violations.length > 0) {
    assert.fail('rule6 violations:\n' + violations.map((v, i) => `  [${i+1}] ${v}`).join('\n'));
  }
});
