import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { copyFile, mkdir, chmod } from 'node:fs/promises';
import { resolve, dirname } from 'node:path';
import { createRequire } from 'node:module';
import { assertAuthoringParity, probeAuthoringContract } from './authoring-contract.mjs';
const require = createRequire(import.meta.url);
const { decodeCapabilities } = require('../out/presentationProtocol.js');
const { decodeAuthoringCapabilities, parseAuthoringJSON } = require('../out/authoringProtocol.js');
const { finiteHelper } = require('../out/presentationClient.js');
const source = process.argv[2];
if (!source) throw new Error('Pass the explicitly built matching helper path; no PATH fallback.');
const binary = resolve(source);
const { stdout } = await promisify(execFile)(binary, ['presentation', 'capabilities', '--v3'], { timeout: 5000, maxBuffer: 8 * 1024 * 1024, windowsHide: true });
decodeCapabilities(JSON.parse(stdout), 3);
decodeAuthoringCapabilities(await finiteHelper(binary, ['authoring', 'capabilities', '--v3'], '', undefined, undefined, parseAuthoringJSON), 3);
const sourceAuthoring = await probeAuthoringContract(binary);
const destination = resolve(import.meta.dirname, '..', 'bin', `${process.platform}-${process.arch}`, process.platform === 'win32' ? 'yawr.exe' : 'yawr');
await mkdir(dirname(destination), { recursive: true });
await copyFile(binary, destination);
if (process.platform !== 'win32') {
  await chmod(destination, 0o755);
}
const packagedAuthoring = await probeAuthoringContract(destination);
assertAuthoringParity(sourceAuthoring, packagedAuthoring);
console.log(`Verified current authoring v3 parity and packaged ${destination}`);
