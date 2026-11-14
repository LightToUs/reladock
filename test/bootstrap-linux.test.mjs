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
 const fakeAgent=[
  '#!/bin/sh',
  '[ "$1" = enroll ] || exit 8',
  'printf "enroll\\n" >> /tmp/enrollments',
  'mkdir /etc/reladock/node-pki',
  'printf "fixture-private" > /etc/reladock/node-pki/agent.key',
  'printf "fixture-enrollment" > /etc/reladock/node-pki/enrollment.json',
  'printf "fixture-certificate" > /etc/reladock/node-pki/agent.crt',
  'printf "fixture-ca" > /etc/reladock/node-pki/ca.crt',
  'printf "fixture-certificatefixture-private" > /etc/reladock/node-pki/identity.pem',
  'printf "https://agent.example.com:9443" > /etc/reladock/node-pki/control',
  'printf "hk-01" > /etc/reladock/node-pki/node-id',
  'printf "RELADOCK_NODE=hk-01\\nRELADOCK_CONTROL=https://agent.example.com:9443\\nRELADOCK_PKI=/etc/reladock/node-pki\\n" > /etc/reladock/agent.env',
 ].join('\n')+'\n';
 const files={'reladock':'fixture control','reladock-admin':'fixture admin','reladock-recovery':'fixture recovery','reladock-agent':fakeAgent,'reladock-dataplane':'#!/bin/sh\nexit 0\n','reladock-updater':'#!/bin/sh\nexit 0\n','reladock.service':'fixture control service','reladock-agent.service':'fixture validation','reladock-agent-apply.service':'fixture opt-in','reladock-updater.service':'fixture updater','reladock-updater.timer':'fixture timer','control.env.example':'fixture control env','agent.env.example':'fixture agent env','install-linux.sh':await readFile(join(process.cwd(),'deploy/install-linux.sh')),'bootstrap-agent.sh':'fixture bootstrap','RELEASE.json':'{}\n','DEPLOYMENT.md':'fixture deployment','LINK-AND-CAPACITY.md':'fixture capacity','OFFSITE-RECOVERY.md':'fixture recovery docs','SAFE-RETIREMENT.md':'fixture retirement'};
 const sums=[];for(const [n,b]of Object.entries(files)){await writeFile(join(dir,'release',n),b);sums.push(`${createHash('sha256').update(b).digest('hex')}  ${n}`)}
 await writeFile(join(dir,'release','SHA256SUMS'),sums.join('\n')+'\n');
 const asset=await packRelease(join(dir,'release'),'0.7.0','amd64',[...Object.keys(files),'SHA256SUMS']);await writeFile(join(dir,'release.tar.gz'),await readFile(asset.path));
 const hash=createHash('sha256').update(await readFile(join(dir,'release.tar.gz'))).digest('hex');
 const curl=`#!/bin/sh\ncase "$*" in *api/node-bootstrap/release*) [ ! -f /tmp/fail-metadata ] || exit 22; echo "v0.7.0 ${hash} ${hash}";; *) while [ "$1" != -o ]; do shift; done; cp /fixture/release.tar.gz "$2";; esac\n`;
 const systemctl=[
  '#!/bin/sh',
  'printf "%s\\n" "$*" >> /tmp/services',
  'case "$1" in enable) unit=$3;; is-active|is-enabled) unit=$3;; *) unit=$2;; esac',
  '[ "$unit" != reladock-agent ] || unit=reladock-agent.service',
  'case "$1" in',
  '  enable) [ "$2" = --now ] || exit 0; touch "/tmp/enabled.$unit" "/tmp/active.$unit";;',
  '  start|restart) if [ -f "/tmp/fail-once.$unit" ]; then rm -f "/tmp/fail-once.$unit"; exit 1; fi; touch "/tmp/active.$unit";;',
  '  stop) rm -f "/tmp/active.$unit";;',
  '  is-active) [ "$2" = --quiet ] && test -f "/tmp/active.$unit";;',
  '  is-enabled) [ "$2" = --quiet ] && test -f "/tmp/enabled.$unit";;',
  'esac',
 ].join('\n')+'\n';
 const tools={curl,systemctl,useradd:'#!/bin/sh\naddgroup -S reladock\nadduser -S -D -G reladock reladock\n',uname:'#!/bin/sh\ncase "$1" in -s)echo Linux;;-m)echo x86_64;;esac\n'};
 for(const[n,b]of Object.entries(tools)){await writeFile(join(dir,'tools',n),b);await chmod(join(dir,'tools',n),0o755)}
 await writeFile(join(dir,'installer'),await readFile(join(process.cwd(),'deploy/bootstrap-agent.sh')));
 const command='export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; printf "%064d\\n" 0 | sh /installer https://console.example.com; test -f /etc/reladock/node-pki/agent.key; grep -q "enable --now reladock-agent$" /tmp/services; grep -q "enable --now reladock-updater.timer$" /tmp/services; ! grep -q "enable.*apply" /tmp/services; key_before=$(sha256sum /etc/reladock/node-pki/agent.key); env_before=$(sha256sum /etc/reladock/agent.env); cp -p /etc/reladock/agent.env /tmp/agent.env.good; printf "RELADOCK_NODE=wrong-node\\nRELADOCK_CONTROL=https://agent.example.com:9443\\nRELADOCK_PKI=/etc/reladock/node-pki\\n" > /etc/reladock/agent.env; if sh /installer https://console.example.com --existing-action upgrade; then exit 9; fi; cp -p /tmp/agent.env.good /etc/reladock/agent.env; printf "old-agent" > /opt/reladock/reladock-agent; touch /tmp/fail-metadata; printf "%064d\\n" 1 | sh /installer https://console.example.com --existing-action none; rm -f /tmp/fail-metadata; grep -q old-agent /opt/reladock/reladock-agent; rm -f /usr/sbin/conntrack; printf "%064d\\n" 2 | sh /installer https://console.example.com --existing-action upgrade; grep -q fixture-certificate /opt/reladock/reladock-agent; systemctl is-active --quiet reladock-agent.service; printf "#!/bin/sh\\necho conntrack v1.4.7\\n" > /usr/sbin/conntrack; chmod 0755 /usr/sbin/conntrack; systemctl stop reladock-agent.service; systemctl enable --now reladock-agent-apply.service; printf "%064d\\n" 3 | sh /installer https://console.example.com --existing-action upgrade; grep -q fixture-certificate /opt/reladock/reladock-agent; test "$(sha256sum /etc/reladock/node-pki/agent.key)" = "$key_before"; test "$(sha256sum /etc/reladock/agent.env)" = "$env_before"; test "$(wc -l < /tmp/enrollments)" = 1; systemctl is-active --quiet reladock-agent-apply.service; ! systemctl is-active --quiet reladock-agent.service; printf "old-good-agent" > /opt/reladock/reladock-agent; touch /tmp/fail-once.reladock-agent-apply.service; if printf "%064d\\n" 4 | sh /installer https://console.example.com --existing-action upgrade; then exit 9; fi; grep -q old-good-agent /opt/reladock/reladock-agent; systemctl is-active --quiet reladock-agent-apply.service; test "$(sha256sum /etc/reladock/node-pki/agent.key)" = "$key_before"; test "$(sha256sum /etc/reladock/agent.env)" = "$env_before"';
 const runContainer=command=>{const fixture=spawnSync('tar',['-cf','-','-C',dir,'tools','installer','release.tar.gz'],{maxBuffer:8*1024*1024});assert.equal(fixture.status,0);return spawnSync('docker',['run','-i','--rm','--network','none','--security-opt','no-new-privileges','--entrypoint','sh','reladock-dataplane-test:local','-ec','mkdir -p /fixture /run/systemd/system /etc/systemd/system; printf "#!/bin/sh\\necho conntrack v1.4.7\\n" > /usr/sbin/conntrack; chmod 0755 /usr/sbin/conntrack; tar -xf - -C /fixture; '+command.replaceAll('/installer','/fixture/installer')],{input:fixture.stdout,encoding:'utf8',timeout:60000})};
 const run=runContainer(command);
 assert.equal(run.status,0,run.stdout+run.stderr);
 const interactiveName=`reladock-bootstrap-menu-${process.pid}-${Date.now()}`;
 t.after(()=>spawnSync('docker',['rm','-f',interactiveName]));
 const interactiveCommand='mkdir -p /run/systemd/system /etc/systemd/system; printf "#!/bin/sh\\necho conntrack v1.4.7\\n" > /usr/sbin/conntrack; chmod 0755 /usr/sbin/conntrack; export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; printf "%064d\\n" 3 | sh /fixture/installer https://console.example.com; printf "old-agent" > /opt/reladock/reladock-agent; sh /fixture/installer https://console.example.com; grep -q old-agent /opt/reladock/reladock-agent; test "$(wc -l < /tmp/enrollments)" = 1';
 const interactiveCreate=spawnSync('docker',['create','-i','-t','--name',interactiveName,'--network','none','--security-opt','no-new-privileges','--entrypoint','sh','reladock-dataplane-test:local','-ec',interactiveCommand],{encoding:'utf8'});
 assert.equal(interactiveCreate.status,0,interactiveCreate.stdout+interactiveCreate.stderr);
 const interactiveCopy=spawnSync('docker',['cp',`${dir}/.`,`${interactiveName}:/fixture`],{encoding:'utf8'});
 assert.equal(interactiveCopy.status,0,interactiveCopy.stdout+interactiveCopy.stderr);
 const interactiveScript=`
set timeout 20
spawn docker start -a -i ${interactiveName}
expect {
 -exact {Select [1/2]: } {}
 eof {exit 91}
 timeout {exit 92}
}
send "2\\r"
expect {
 -exact {Existing complete Agent installation left unchanged.} {}
 eof {exit 93}
 timeout {exit 94}
}
expect eof
set result [wait]
exit [lindex $result 3]
`;
 const interactive=spawnSync('/usr/bin/expect',['-c',interactiveScript],{encoding:'utf8',timeout:30000});
 assert.equal(interactive.status,0,interactive.stdout+interactive.stderr);
 const partial=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; mkdir -p /etc/reladock; printf "RELADOCK_NODE=partial\\n" > /etc/reladock/agent.env; chmod 0600 /etc/reladock/agent.env; if sh /installer https://console.example.com --existing-action upgrade; then exit 9; fi; test ! -e /opt/reladock/reladock-agent');
 assert.equal(partial.status,0,partial.stdout+partial.stderr);
 assert.match(partial.stderr,/Existing Agent state is incomplete or conflicting/);
 const binaryOnly=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; mkdir -p /opt/reladock; printf "partial" > /opt/reladock/reladock-agent; if sh /installer https://console.example.com; then exit 9; fi');
 assert.equal(binaryOnly.status,0,binaryOnly.stdout+binaryOnly.stderr);
 assert.match(binaryOnly.stderr,/Existing Agent state is incomplete or conflicting/);
 const noninteractive=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; printf "%064d\\n" 5 | sh /installer https://console.example.com; printf "old-agent" > /opt/reladock/reladock-agent; sh /installer https://console.example.com; grep -q old-agent /opt/reladock/reladock-agent; test "$(wc -l < /tmp/enrollments)" = 1');
 assert.equal(noninteractive.status,0,noninteractive.stdout+noninteractive.stderr);
 assert.match(noninteractive.stdout,/left unchanged because no interactive terminal is available/);
 // Same trusted digest, but a symlink inside the archive: reject before install.
 await rm(join(dir,'release','reladock-agent'));await symlink('/tmp/escape',join(dir,'release','reladock-agent'));
 spawnSync('tar',['--format=ustar','-czf',join(dir,'release.tar.gz'),'-C',join(dir,'release'),...Object.keys(files),'SHA256SUMS'],{env:{...process.env,COPYFILE_DISABLE:'1'}});
 const badHash=createHash('sha256').update(await readFile(join(dir,'release.tar.gz'))).digest('hex');
 await writeFile(join(dir,'tools','curl'),curl.replaceAll(hash,badHash));
 const bad=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; ! sh /installer https://console.example.com; test ! -e /opt/reladock/reladock-agent');assert.equal(bad.status,0,bad.stdout+bad.stderr);
 await rm(join(dir,'release','reladock-agent'));await writeFile(join(dir,'release','reladock-agent'),fakeAgent);
 await rm(join(dir,'release','reladock'));await mkdir(join(dir,'release','reladock'));await writeFile(join(dir,'release','reladock','service'),'nested regular file');
 spawnSync('tar',['--format=ustar','-czf',join(dir,'release.tar.gz'),'-C',join(dir,'release'),...Object.keys(files),'SHA256SUMS','reladock/service'],{env:{...process.env,COPYFILE_DISABLE:'1'}});
 const nestedHash=createHash('sha256').update(await readFile(join(dir,'release.tar.gz'))).digest('hex');await writeFile(join(dir,'tools','curl'),curl.replaceAll(hash,nestedHash));
 const nested=runContainer('export PATH=/fixture/tools:/usr/sbin:/usr/bin:/sbin:/bin; if sh /installer https://console.example.com; then exit 9; fi; test ! -e /opt/reladock/reladock-agent');assert.equal(nested.status,0,nested.stdout+nested.stderr);assert.match(nested.stderr,/Incomplete or unsafe archive paths/);
});
