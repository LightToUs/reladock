import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,mkdir,writeFile,readFile,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
test('release packaging produces a flat pinned asset with independently verifiable SHA256',async t=>{
 const {packRelease}=await import('../scripts/release-package.mjs');
 const root=await mkdtemp(join(tmpdir(),'reladock-pack-'));t.after(()=>rm(root,{recursive:true,force:true}));const dir=join(root,'release');await mkdir(dir);
 await writeFile(join(dir,'reladock-agent'),'fixture');await writeFile(join(dir,'SHA256SUMS'),'fixture');
 const asset=await packRelease(dir,'0.7.0','arm64',['reladock-agent','SHA256SUMS']);
 assert.equal(asset.name,'reladock-v0.7.0-linux-arm64.tar.gz');
 const sum=createHash('sha256').update(await readFile(asset.path)).digest('hex');assert.equal(sum,asset.sha256);
 const listing=spawnSync('tar',['-tzf',asset.path],{encoding:'utf8'});assert.equal(listing.status,0);assert.deepEqual(listing.stdout.trim().split('\n').sort(),['SHA256SUMS','reladock-agent']);
 await assert.rejects(()=>packRelease(dir,'0.7.0','arm64',['../escape']));
});

test('production release manifest includes the hardened root updater',async()=>{
 const {releaseFiles}=await import('../scripts/release-package.mjs');
 assert.deepEqual(releaseFiles.filter(name=>name.includes('updater')),['reladock-updater','reladock-updater.service','reladock-updater.timer']);
 assert.equal(new Set(releaseFiles).size,releaseFiles.length);
 const service=await readFile(join(process.cwd(),'deploy','reladock-updater.service'),'utf8');
 assert.match(service,/^ExecStart=\/opt\/reladock\/reladock-updater$/m);
 assert.match(service,/^User=root$/m);
 assert.match(service,/^Group=reladock$/m);
 assert.match(service,/^ProtectSystem=strict$/m);
 assert.match(service,/^TimeoutStartSec=10min$/m);
 assert.match(service,/^CapabilityBoundingSet=CAP_DAC_READ_SEARCH$/m);
 assert.match(service,/^AmbientCapabilities=CAP_DAC_READ_SEARCH$/m);
 assert.doesNotMatch(service,/CAP_NET_ADMIN|bash|sh -c/);
 const timer=await readFile(join(process.cwd(),'deploy','reladock-updater.timer'),'utf8');
 assert.match(timer,/^RandomizedDelaySec=/m);
 const bootstrap=await readFile(join(process.cwd(),'deploy','bootstrap-agent.sh'),'utf8');
 const installer=await readFile(join(process.cwd(),'deploy','install-linux.sh'),'utf8');
 for(const script of [bootstrap,installer]){
  assert.match(script,/install -d -o reladock -g reladock -m 0700 \/var\/lib\/reladock-agent\/control/);
  assert.match(script,/install -o root -g reladock -m 0640 \/dev\/null \/var\/lib\/reladock-locks\/control\.lock/);
 }
});
