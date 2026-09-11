import { readFile, rename, rm, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import JSZip from 'jszip';

const vsixPath = resolve(process.argv[2] ?? 'yawr-preview.vsix');
const outputPath = `${vsixPath}.deterministic`;
await rm(outputPath, { force: true });
const epoch = new Date('1980-01-01T00:00:00.000Z');
const source = await JSZip.loadAsync(await readFile(vsixPath));
const normalized = new JSZip();

for (const name of Object.keys(source.files).sort()) {
  const entry = source.files[name];
  const data = entry.dir ? new Uint8Array() : await entry.async('uint8array');
  normalized.file(name, data, {
    binary: true,
    createFolders: false,
    date: epoch,
    dir: entry.dir,
    unixPermissions: entry.unixPermissions ?? (entry.dir ? 0o40755 : 0o100644),
    compression: entry.dir ? 'STORE' : 'DEFLATE',
    compressionOptions: entry.dir ? undefined : { level: 9 },
  });
}

const archive = await normalized.generateAsync({
  type: 'nodebuffer',
  platform: 'UNIX',
  compression: 'DEFLATE',
  compressionOptions: { level: 9 },
});

try {
  await writeFile(outputPath, archive);
  await rm(vsixPath, { force: true });
  await rename(outputPath, vsixPath);
} finally {
  await rm(outputPath, { force: true });
}
