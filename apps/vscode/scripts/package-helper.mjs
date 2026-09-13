import { execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { promisify } from 'node:util';
import { existsSync } from 'node:fs';
import { copyFile, mkdir, chmod, readFile, writeFile } from 'node:fs/promises';
import { resolve, dirname, join } from 'node:path';
import { createRequire } from 'node:module';
import { assertAuthoringParity, probeAuthoringContract } from './authoring-contract.mjs';
const execFileAsync = promisify(execFile);
const require = createRequire(import.meta.url);
const { decodeCapabilities } = require('../out/presentationProtocol.js');
const { decodeAuthoringCapabilities, parseAuthoringJSON } = require('../out/authoringProtocol.js');
const { finiteHelper } = require('../out/presentationClient.js');
const hashFile = async (path) => createHash('sha256').update(await readFile(path)).digest('hex');
const source = process.argv[2];
if (!source) throw new Error('Pass the explicitly built matching helper path; no PATH fallback.');
const binary = resolve(source);
const { stdout } = await execFileAsync(binary, ['presentation', 'capabilities', '--v3'], { timeout: 5000, maxBuffer: 8 * 1024 * 1024, windowsHide: true });
decodeCapabilities(JSON.parse(stdout), 3);
decodeAuthoringCapabilities(await finiteHelper(binary, ['authoring', 'capabilities', '--v3'], '', undefined, undefined, parseAuthoringJSON), 3);
const sourceAuthoring = await probeAuthoringContract(binary);
const destination = resolve(import.meta.dirname, '..', 'bin', `${process.platform}-${process.arch}`, process.platform === 'win32' ? 'yawr.exe' : 'yawr');
const expectedHelperSHA256 = await hashFile(binary);
await mkdir(dirname(destination), { recursive: true });
await copyFile(binary, destination);
if (process.platform !== 'win32') {
  await chmod(destination, 0o755);
}
const packagedAuthoring = await probeAuthoringContract(destination);
assertAuthoringParity(sourceAuthoring, packagedAuthoring);
if (await hashFile(destination) !== expectedHelperSHA256) {
  throw new Error('packaged helper SHA-256 differs from the exact source helper input');
}

const fixtureDirectory = resolve(import.meta.dirname, '..', 'fixtures', 'file-only-subprocess', 'win32-x64');
const fixtureSource = resolve(import.meta.dirname, '..', '..', '..', 'runtime', 'internal', 'tool', 'testdata', 'fileonlyfixture', 'main.go');
const fixture = resolve(fixtureDirectory, 'fixture.exe');
const windowsGoCandidates = [
  process.env.GOROOT ? join(process.env.GOROOT, 'bin', 'go.exe') : '',
  process.env.ProgramFiles ? join(process.env.ProgramFiles, 'Go', 'bin', 'go.exe') : '',
  process.env.LOCALAPPDATA ? join(process.env.LOCALAPPDATA, 'Programs', 'Go', 'bin', 'go.exe') : '',
  'C:\\Go\\bin\\go.exe',
].filter(Boolean);
const go = process.platform === 'win32'
  ? windowsGoCandidates.find((candidate) => existsSync(candidate)) ?? 'go'
  : 'go';
await mkdir(fixtureDirectory, { recursive: true });
await execFileAsync(go, [
  'build',
  '-trimpath',
  '-buildvcs=false',
  '-o',
  fixture,
  fixtureSource,
], {
  env: { ...process.env, GOOS: 'windows', GOARCH: 'amd64', CGO_ENABLED: '0' },
  timeout: 120_000,
  maxBuffer: 8 * 1024 * 1024,
  windowsHide: true,
});
await writeFile(resolve(fixtureDirectory, 'input.txt'), 'installed-staged-value');
const expectedFixtureSHA256 = await hashFile(fixture);
console.log(`Verified current authoring v3 parity and packaged ${destination} sha256=${expectedHelperSHA256} with ${fixture} sha256=${expectedFixtureSHA256}`);
