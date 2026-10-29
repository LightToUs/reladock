import { createWriteStream, createReadStream } from 'node:fs';
import { mkdir, realpath, stat } from 'node:fs/promises';
import { pipeline } from 'node:stream/promises';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { resolve, sep } from 'node:path';

const root=fileURLToPath(new URL('../',import.meta.url));
const directory=resolve(root,'.local/backups');
const mode=process.argv[2];
await mkdir(directory,{recursive:true,mode:0o700});
function command(args,{input,output,capture=false}={}){
  const child=spawn('docker',['compose','exec','-T','postgres',...args],{cwd:root,stdio:[input?'pipe':'ignore',output||capture?'pipe':'ignore','inherit']});
  let result='';if(capture)child.stdout.setEncoding('utf8').on('data',chunk=>{result+=chunk;});
  const done=new Promise((ok,fail)=>{child.once('error',fail);child.once('exit',code=>code===0?ok():fail(new Error(`Database tool failed (${code}); no success claimed`)));});
  const streams=[];if(input)streams.push(pipeline(input,child.stdin));if(output)streams.push(pipeline(child.stdout,output));
  return Promise.all([done,...streams]).then(()=>result);
}
if(mode==='create'){
  const path=resolve(directory,`reladock-${Date.now()}-${randomBytes(6).toString('hex')}.dump`);
  await command(['pg_dump','-U','reladock','-d','reladock','--format=custom'],{output:createWriteStream(path,{flags:'wx',mode:0o600})});
  const info=await stat(path);if(info.size===0)throw new Error('Empty backup');
  console.log(`Local backup created (${info.size} bytes): ${path}`);
  console.log('Restore verification has not run. Keep this file private.');
}else if(mode==='verify'){
  if(!process.argv[3])throw new Error('Supply an absolute .local/backups/*.dump path created by this project');
  const path=await realpath(process.argv[3]);const base=await realpath(directory);
  if(!path.startsWith(base+sep)||!path.endsWith('.dump')||!(await stat(path)).isFile())throw new Error('Only this project’s local backup files are supported');
  const database=`reladock_restore_${randomBytes(12).toString('hex')}`;
  await command(['createdb','-U','reladock',database]);
  try{
    await command(['pg_restore','-U','reladock','--dbname',database,'--no-owner','--exit-on-error'],{input:createReadStream(path)});
    const sql=`DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM schema_migrations) THEN RAISE EXCEPTION 'missing schema history'; END IF; IF EXISTS(SELECT 1 FROM billing_wallets w WHERE w.balance_cents<>(SELECT COALESCE(sum(amount_cents),0) FROM billing_ledger l WHERE l.tenant_id=w.tenant_id)) THEN RAISE EXCEPTION 'wallet/ledger mismatch'; END IF; END $$; SELECT max(version) AS restored_schema_version FROM schema_migrations;`;
    const result=await command(['psql','-U','reladock','-d',database,'-v','ON_ERROR_STOP=1','-At','-c',sql],{capture:true});
    console.log(`Local restore and wallet/ledger consistency verified. Schema: ${result.trim().split('\n').at(-1)}`);
  }finally{
    await command(['dropdb','-U','reladock','--force',database]);
    console.log(`Removed only the temporary verification database ${database}. The original database and backup are preserved.`);
  }
}else{throw new Error('Mode must be create or verify');}
