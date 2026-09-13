import { access, readFile } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const read = (relative) =>
  readFile(resolve(root, ...relative.split('/')), 'utf8');

const repository = JSON.parse(await read('package.json'));
const extension = JSON.parse(await read('apps/vscode/package.json'));
const goModule = await read('runtime/go.mod');
const workflow = await read('.github/workflows/ci.yml');

if (
  repository.name !== '@yawr/repository' ||
  !repository.workspaces?.includes('apps/vscode')
) {
  throw new Error('Root npm workspace identity is not canonical Yawr.');
}
if (
  extension.name !== 'yawr-preview' ||
  !extension.scripts?.package?.includes('yawr-preview.vsix')
) {
  throw new Error('Extension package or VSIX identity is not canonical Yawr.');
}
if (!/^module github\.com\/ormasoftchile\/yawr\/runtime\r?\n/.test(goModule)) {
  throw new Error('Runtime Go module identity is not canonical Yawr.');
}

await Promise.all([
  access(resolve(root, 'runtime', 'cmd', 'yawr')),
]);

for (const token of [
  './cmd/yawr',
  'YAWR_CORE_ROOT',
  'YAWR_AUTHORING_HELPER',
  'YAWR_EXPRESSION_HELPER',
  'YAWR_E2E_BINARY',
  'extension:package:validate',
]) {
  if (!workflow.includes(token)) {
    throw new Error(`Root CI is missing required Yawr token: ${token}`);
  }
}

const runtime = workflow.match(
  /^  runtime:\r?\n(?<job>[\s\S]*?)(?=^  [a-z][a-z0-9-]+:\r?$)/m,
)?.groups?.job;
if (!runtime) {
  throw new Error('Root CI is missing the runtime job.');
}
if (!/timeout-minutes:\s*15/.test(runtime)) {
  throw new Error('Runtime CI must remain bounded by the repository job budget.');
}
if (!runtime.includes('go test -p 1 ./... -count=1')) {
  throw new Error('Runtime CI must serialize packages while retaining the complete uncached suite.');
}
if (/\b-run\b|\b-skip\b/.test(runtime)) {
  throw new Error('Runtime CI must not filter or skip tests.');
}

const extensionUnit = workflow.match(
  /^  extension-unit:\r?\n(?<job>[\s\S]*?)(?=^  [a-z][a-z0-9-]+:\r?$)/m,
)?.groups?.job;
if (!extensionUnit) {
  throw new Error('Root CI is missing the extension-unit job.');
}

const orderedExtensionUnitTokens = [
  'matrix:',
  'os: [ubuntu-latest, macos-latest, windows-latest]',
  'actions/setup-go@',
  "go-version: '1.25.7'",
  'Build and export current expression helper',
  "if ($IsWindows) { 'yawr-expression.exe' } else { 'yawr-expression' }",
  'go build -C runtime -trimpath -buildvcs=false -o $helper ./cmd/yawr',
  'Test-Path -LiteralPath $helper -PathType Leaf',
  'github\\.com/ormasoftchile/yawr/runtime/cmd/yawr',
  'YAWR_EXPRESSION_HELPER=$helper',
  '$env:GITHUB_ENV',
  'Run complete retained unit suite',
  'npm run extension:test',
];
let previousIndex = -1;
for (const token of orderedExtensionUnitTokens) {
  const index = extensionUnit.indexOf(token);
  if (index < 0) {
    throw new Error(`Extension-unit CI is missing helper provisioning token: ${token}`);
  }
  if (index <= previousIndex) {
    throw new Error(`Extension-unit CI helper provisioning is out of order at: ${token}`);
  }
  previousIndex = index;
}
if (
  (extensionUnit.match(/go build -C runtime/g) ?? []).length !== 1 ||
  (extensionUnit.match(/npm run extension:test/g) ?? []).length !== 1
) {
  throw new Error('Each extension-unit matrix job must build one helper and run the retained suite once.');
}

console.log('Verified Yawr-only root wiring.');
