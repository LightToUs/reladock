import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {join} from 'node:path';

test('agent installer and apply service require the fixed conntrack executable',async()=>{
 const installer=await readFile(join(process.cwd(),'deploy','install-linux.sh'),'utf8');
 assert.match(installer,/\/usr\/sbin\/conntrack/);
 assert.match(installer,/conntrack-tools|apt-get install -y conntrack/);
 assert.match(installer,/Unable to install conntrack tools/);
 const apply=await readFile(join(process.cwd(),'deploy','reladock-agent-apply.service'),'utf8');
 assert.match(apply,/^ExecStartPre=\/usr\/sbin\/conntrack --version$/m);
 assert.match(apply,/^CapabilityBoundingSet=CAP_NET_ADMIN$/m);
 const validation=await readFile(join(process.cwd(),'deploy','reladock-agent.service'),'utf8');
 assert.doesNotMatch(validation,/^ExecStartPre=.*conntrack/m);
 assert.match(validation,/^CapabilityBoundingSet=$/m);
 assert.doesNotMatch(validation,/^AmbientCapabilities=CAP_NET_ADMIN$/m);
 assert.doesNotMatch(installer,/systemctl enable --now reladock-agent-apply/);
});
