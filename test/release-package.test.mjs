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
