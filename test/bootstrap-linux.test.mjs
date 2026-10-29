import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,mkdir,writeFile,readFile,chmod,symlink,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {packRelease} from '../scripts/release-package.mjs';
const enabled=process.env.RELADOCK_INSTALLER_DOCKER_TESTS==='1';
test('Linux installer verifies archive, enrolls, enables only validation, and protects existing state',{skip:!enabled},async t=>{
 const dir=await mkdtemp('/private/tmp/reladock-linux-install-');t.after(()=>rm(dir,{recursive:true,force:true}));
 await mkdir(join(dir,'tools'));await mkdir(join(dir,'release'));
 const fakeAgent='#!/bin/sh\n[ "$1" = enroll ] || exit 8\nmkdir /etc/reladock/node-pki\nprintf "fixture-private" > /etc/reladock/node-pki/agent.key\nprintf "RELADOCK_NODE=hk-01\\n" > /etc/reladock/agent.env\n';
 const files={'reladock-agent':fakeAgent,'reladock-dataplane':'#!/bin/sh\nexit 0\n','reladock-agent.service':'fixture validation','reladock-agent-apply.service':'fixture opt-in'};
 const sums=[];for(const [n,b]of Object.entries(files)){await writeFile(join(dir,'release',n),b);sums.push(`${createHash('sha256').update(b).digest('hex')}  ${n}`)}
 await writeFile(join(dir,'release','SHA256SUMS'),sums.join('\n')+'\n');
 const asset=await packRelease(join(dir,'release'),'0.7.0','amd64',[...Object.keys(files),'SHA256SUMS']);await writeFile(join(dir,'release.tar.gz'),await readFile(asset.path));
 const hash=createHash('sha256').update(await readFile(join(dir,'release.tar.gz'))).digest('hex');
 const curl=`#!/bin/sh\ncase "$*" in *api/node-bootstrap/release*) echo "v0.7.0 ${hash} ${hash}";; *) while [ "$1" != -o ]; do shift; done; cp /fixture/release.tar.gz "$2";; esac\n`;
 const tools={curl,systemctl:'#!/bin/sh\nprintf "%s\\n" "$*" >> /tmp/services\n',useradd:'#!/bin/sh\naddgroup -S reladock\nadduser -S -D -G reladock reladock\n',uname:'#!/bin/sh\ncase "$1" in -s)echo Linux;;-m)echo x86_64;;esac\n'};
 for(const[n,b]of Object.entries(tools)){await writeFile(join(dir,'tools',n),b);await chmod(join(dir,'tools',n),0o755)}
 await writeFile(join(dir,'installer'),await readFile(join(process.cwd(),'deploy/bootstrap-agent.sh')));
 const command='export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; printf "%064d\\n" 0 | sh /installer https://console.example.com; test -f /etc/reladock/node-pki/agent.key; grep -q "enable --now reladock-agent$" /tmp/services; ! grep -q "enable.*apply" /tmp/services; ! sh /installer https://console.example.com; grep -q fixture-private /etc/reladock/node-pki/agent.key';
 const runContainer=command=>{const fixture=spawnSync('tar',['-cf','-','-C',dir,'tools','installer','release.tar.gz'],{maxBuffer:8*1024*1024});assert.equal(fixture.status,0);return spawnSync('docker',['run','-i','--rm','--network','none','--security-opt','no-new-privileges','--entrypoint','sh','reladock-dataplane-test:local','-ec','mkdir -p /fixture /run/systemd/system /etc/systemd/system; tar -xf - -C /fixture; '+command.replaceAll('/installer','/fixture/installer')],{input:fixture.stdout,encoding:'utf8',timeout:60000})};
 const run=runContainer(command);
 assert.equal(run.status,0,run.stdout+run.stderr);
 // Same trusted digest, but a symlink inside the archive: reject before install.
 await rm(join(dir,'release','reladock-agent'));await symlink('/tmp/escape',join(dir,'release','reladock-agent'));
 spawnSync('tar',['--format=ustar','-czf',join(dir,'release.tar.gz'),'-C',join(dir,'release'),...Object.keys(files),'SHA256SUMS'],{env:{...process.env,COPYFILE_DISABLE:'1'}});
 const badHash=createHash('sha256').update(await readFile(join(dir,'release.tar.gz'))).digest('hex');
 await writeFile(join(dir,'tools','curl'),curl.replaceAll(hash,badHash));
 const bad=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; ! sh /installer https://console.example.com; test ! -e /opt/reladock/reladock-agent');assert.equal(bad.status,0,bad.stdout+bad.stderr);
 await rm(join(dir,'release','reladock-agent'));await writeFile(join(dir,'release','reladock-agent'),fakeAgent);
 await mkdir(join(dir,'release','reladock'));await writeFile(join(dir,'release','reladock','service'),'nested regular file');
 spawnSync('tar',['--format=ustar','-czf',join(dir,'release.tar.gz'),'-C',join(dir,'release'),...Object.keys(files),'SHA256SUMS','reladock/service'],{env:{...process.env,COPYFILE_DISABLE:'1'}});
 const nestedHash=createHash('sha256').update(await readFile(join(dir,'release.tar.gz'))).digest('hex');await writeFile(join(dir,'tools','curl'),curl.replaceAll(hash,nestedHash));
 const nested=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; if sh /installer https://console.example.com; then exit 9; fi; test ! -e /opt/reladock/reladock-agent');assert.equal(nested.status,0,nested.stdout+nested.stderr);assert.match(nested.stderr,/Unsafe archive paths/);
});
