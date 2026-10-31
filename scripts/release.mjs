import { mkdir, copyFile, readFile, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import {packRelease,releaseFiles} from './release-package.mjs';

const root = fileURLToPath(new URL('../', import.meta.url));
const arch = process.env.RELEASE_ARCH || 'amd64';
if (!['amd64', 'arm64'].includes(arch)) throw new Error('RELEASE_ARCH must be amd64 or arm64');
const output = new URL(`../.cache/release/linux-${arch}/`, import.meta.url);
await mkdir(output, { recursive: true });
const metadata=JSON.parse(await readFile(new URL('../package.json',import.meta.url),'utf8'));
const releaseVersion=`v${metadata.version}`;
const programs = ['reladock', 'reladock-admin', 'reladock-agent', 'reladock-dataplane', 'reladock-updater'];
const hashes = [];
for (const program of programs) {
  const path = fileURLToPath(new URL(program, output));
  const result = spawnSync('go', ['build', '-trimpath', '-ldflags', `-X reladock/internal/buildinfo.ReleaseVersion=${releaseVersion}`, '-o', path, `./cmd/${program}`], {
    cwd: root, stdio: 'inherit', env: { ...process.env, CGO_ENABLED: '0', GOOS: 'linux', GOARCH: arch, GOCACHE: process.env.GOCACHE || `${root}.cache/go-build` },
  });
  if (result.status !== 0) process.exit(result.status || 1);
  hashes.push(`${createHash('sha256').update(await readFile(path)).digest('hex')}  ${program}`);
}
for (const name of ['reladock.service', 'reladock-agent.service','reladock-agent-apply.service','reladock-updater.service','reladock-updater.timer', 'control.env.example', 'agent.env.example','install-linux.sh','bootstrap-agent.sh']) {
  await copyFile(new URL(`../deploy/${name}`, import.meta.url), new URL(name, output));
  hashes.push(`${createHash('sha256').update(await readFile(new URL(name,output))).digest('hex')}  ${name}`);
}
await copyFile(new URL('../docs/DEPLOYMENT.md', import.meta.url), new URL('DEPLOYMENT.md', output));
hashes.push(`${createHash('sha256').update(await readFile(new URL('DEPLOYMENT.md',output))).digest('hex')}  DEPLOYMENT.md`);
await writeFile(new URL('RELEASE.json',output),JSON.stringify({name:'RelaDock',version:metadata.version,os:'linux',arch,productionReady:false,agentMode:'validation-default; explicit composed Linux application',unverified:['production VPS acceptance and capacity','end-to-end cooperative protocol health','merchant activation and reconciliation','offsite disaster recovery','privileged full safe-retirement lifecycle on a production-like VPS']},null,2)+'\n');
hashes.push(`${createHash('sha256').update(await readFile(new URL('RELEASE.json',output))).digest('hex')}  RELEASE.json`);
await writeFile(new URL('SHA256SUMS', output), `${hashes.join('\n')}\n`);
const asset=await packRelease(fileURLToPath(output).replace(/\/$/,''),metadata.version,arch,releaseFiles);
console.log(`GitHub Release asset: ${asset.path}\nSHA256: ${asset.sha256}`);
console.log(`Linux ${arch} binaries and service templates: ${fileURLToPath(output)}`);
console.log('Build artifacts only. No server was contacted or installed.');
