import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,writeFile,chmod,readFile,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';

// Runs only preflight mode: never installs files, starts services or changes networking.
async function sandbox(t, arch, metadata) {
 const dir=await mkdtemp(join(tmpdir(),'reladock-install-test-'));t.after(()=>rm(dir,{recursive:true,force:true}));
 const stubs={uname:`#!/bin/sh\ncase "$1" in -s) echo Linux;; -m) echo '${arch}';; esac\n`,id:'#!/bin/sh\necho 0\n',systemctl:'#!/bin/sh\nexit 0\n',curl:`#!/bin/sh\nprintf '%s\\n' '${metadata}'\n`};
 stubs.useradd='#!/bin/sh\nexit 0\n';
 for(const [name,body] of Object.entries(stubs)){await writeFile(join(dir,name),body);await chmod(join(dir,name),0o700)}
 return spawnSync('sh',['deploy/bootstrap-agent.sh','https://console.example.com','--check'],{cwd:process.cwd(),env:{...process.env,PATH:`${dir}:/usr/bin:/bin:/usr/sbin:/sbin`},input:'a'.repeat(64)+'\n',encoding:'utf8'});
}
test('bootstrap preflight maps supported architectures to the pinned GitHub release',async t=>{
 for(const [arch,want] of [['x86_64','amd64'],['aarch64','arm64']]){
  const r=await sandbox(t,arch,`v0.7.0 ${'a'.repeat(64)} ${'b'.repeat(64)}`);
  assert.equal(r.status,0,r.stderr);assert.match(r.stdout,new RegExp(`https://github.com/LightToUs/reladock/releases/download/v0.7.0/reladock-v0.7.0-linux-${want}\\.tar\\.gz`));
 }
});
test('bootstrap preflight rejects unsafe metadata and unsupported architecture',async t=>{
 for(const [arch,metadata] of [['mips',`v0.7.0 ${'a'.repeat(64)} ${'b'.repeat(64)}`],['x86_64','v0.7.0 invalid invalid'],['x86_64',`latest ${'a'.repeat(64)} ${'b'.repeat(64)}`]]){
 const r=await sandbox(t,arch,metadata);assert.notEqual(r.status,0);assert.doesNotMatch(r.stdout,/ready/);
 }
});
