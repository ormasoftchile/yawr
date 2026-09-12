// previewOriginHelper.js — regression helper for TestServedPreviewEmptyReferrer.
// Extracts previewParentTargetOrigin() from preview.html and evaluates it under
// a controlled mock browser environment supplied via command-line arguments.
//
// Usage:
//   node previewOriginHelper.js '<scenario-json>' '<absolute-path-to-preview.html>'
//
// Output: one JSON line on stdout: { "result": "<origin>" | null }
// Exit 0 on success, non-zero on error.
//
// No 'use strict' directive — function declarations from eval() must be visible
// in the enclosing scope (standard non-strict V8 behaviour in CJS modules).

const [,, scenarioArg, htmlPath] = process.argv;
if (!scenarioArg || !htmlPath) {
  process.stderr.write('usage: node previewOriginHelper.js <scenario-json> <html-path>\n');
  process.exit(1);
}

const scenario = JSON.parse(scenarioArg);

// --- mock browser globals used by previewParentTargetOrigin() ----------------

// window: isTopLevel → window.parent === window; framed → distinct parent.
globalThis.window = {};
if (scenario.isTopLevel) {
  globalThis.window.parent = globalThis.window;
} else {
  globalThis.window.parent = {}; // a distinct object != window
}

globalThis.location = {
  ancestorOrigins: Array.from(scenario.ancestorOrigins || []),
  search: scenario.search || '',
  origin: scenario.origin || 'http://127.0.0.1:8080',
};
globalThis.document = { referrer: scenario.referrer || '' };

// URL and URLSearchParams are built-in globals in Node.js ≥ 10.

// --- extract previewParentTargetOrigin() from preview.html ------------------

const html = require('fs').readFileSync(htmlPath, 'utf8');
const marker = 'function previewParentTargetOrigin()';
const start = html.indexOf(marker);
if (start < 0) {
  process.stderr.write('previewParentTargetOrigin() not found in ' + htmlPath + '\n');
  process.exit(2);
}

// Brace-count from the first '{' to the matching '}' to extract the body.
const braceStart = html.indexOf('{', start);
let depth = 0, pos = braceStart;
while (pos < html.length) {
  const c = html[pos];
  if (c === '{') depth++;
  else if (c === '}') {
    depth--;
    if (depth === 0) { pos++; break; }
  }
  pos++;
}

const fnSource = html.slice(start, pos);
eval(fnSource); // eslint-disable-line no-eval — defines previewParentTargetOrigin in scope

const result = previewParentTargetOrigin(); // eslint-disable-line no-undef
process.stdout.write(JSON.stringify({ result: result !== undefined ? result : null }) + '\n');
