import { resolve } from 'node:path';
import { createRequire } from 'node:module';
import { assertAuthoringParity, probeAuthoringContract } from './authoring-contract.mjs';

const require = createRequire(import.meta.url);
const { bundledPresentationHelper } = require('../out/presentationClient.js');
const source = process.argv[2];
if (!source) throw new Error('Pass the explicitly built source helper path.');

const sourceBehavior = await probeAuthoringContract(resolve(source));
const packaged = bundledPresentationHelper(resolve(import.meta.dirname, '..'));
const packagedBehavior = await probeAuthoringContract(packaged);
assertAuthoringParity(sourceBehavior, packagedBehavior);
console.log('Source and packaged authoring v3 behavior match; obsolete invocation is rejected.');
