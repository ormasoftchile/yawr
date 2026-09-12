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
  'YAWR_E2E_BINARY',
  'extension:package:validate',
]) {
  if (!workflow.includes(token)) {
    throw new Error(`Root CI is missing required Yawr token: ${token}`);
  }
}

console.log('Verified Yawr-only root wiring.');
