import test from 'node:test';
import assert from 'node:assert/strict';
test('late generation cannot restore cleared credentials or replace a different node',async()=>{
 const {createBootstrapSession}=await import('../public/node-bootstrap.js');const pending=[];
 const session=createBootstrapSession({api:()=>new Promise(resolve=>pending.push(resolve)),copy:async()=>{}});
 const a=session.generate({id:'hk-a',revision:1});session.clear();pending[0]({command:'A',expiresAt:new Date(Date.now()+60000).toISOString()});await a;assert.equal(session.current(),null);
 const b=session.generate({id:'hk-b',revision:1});const c=session.generate({id:'hk-c',revision:1});pending[2]({command:'C',expiresAt:new Date(Date.now()+60000).toISOString()});await c;pending[1]({command:'B',expiresAt:new Date(Date.now()+60000).toISOString()});await b;
 assert.equal(session.current().command,'C');assert.equal(session.current().nodeId,'hk-c');
});
test('install command expires in memory and cannot be copied after expiry',async()=>{
 const {createBootstrapSession}=await import('../public/node-bootstrap.js');let now=1000;let copied='';
 const session=createBootstrapSession({api:async()=>({command:'safe fixture command',expiresAt:new Date(2000).toISOString(),revision:2}),copy:async s=>{copied=s},now:()=>now});
 await session.generate({id:'hk-01',revision:1});await session.copy();assert.equal(copied,'safe fixture command');
 now=2001;assert.equal(session.current(),null);await assert.rejects(()=>session.copy(),/过期/);
});
test('regeneration failure clears old command and revoke uses current revision',async()=>{
 const {createBootstrapSession}=await import('../public/node-bootstrap.js');let fail=false;const requests=[];
 const session=createBootstrapSession({api:async(path,input)=>{requests.push([path,input]);if(fail)throw new Error('未配置');return {command:'fixture',expiresAt:new Date(Date.now()+60000).toISOString(),revision:8}},copy:async()=>{throw new Error('denied')}});
 await session.generate({id:'hk-01',revision:7});await assert.rejects(()=>session.copy(),/复制失败/);
 await session.revoke({id:'hk-01',revision:9});assert.deepEqual(requests[1],['/api/admin/nodes/hk-01/bootstrap-revoke',{revision:9}]);assert.equal(session.current(),null);
 fail=true;await assert.rejects(()=>session.generate({id:'hk-01',revision:9}));assert.equal(session.current(),null);
});
