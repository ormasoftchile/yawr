import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { copyFile, mkdir, chmod } from 'node:fs/promises';
import { resolve, dirname } from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { decodeCapabilities } = require('../out/presentationProtocol.js');
const { decodeAuthoringCapabilities, parseAuthoringJSON } = require('../out/authoringProtocol.js');
const { finiteHelper } = require('../out/presentationClient.js');
const source = process.argv[2];
if (!source) throw new Error('Pass the explicitly built matching helper path; no PATH fallback.');
const binary = resolve(source);
const { stdout } = await promisify(execFile)(binary, ['presentation', 'capabilities'], { timeout: 5000, maxBuffer: 8 * 1024 * 1024, windowsHide: true });
decodeCapabilities(JSON.parse(stdout));
decodeAuthoringCapabilities(await finiteHelper(binary, ['authoring', 'capabilities'], '', undefined, undefined, parseAuthoringJSON));
const destination = resolve(import.meta.dirname, '..', 'bin', `${process.platform}-${process.arch}`, process.platform === 'win32' ? 'yawr.exe' : 'yawr');
await mkdir(dirname(destination), { recursive: true });
await copyFile(binary, destination);
if (process.platform !== 'win32') {
  await chmod(destination, 0o755);
}
console.log(`Verified and packaged ${destination}`);
