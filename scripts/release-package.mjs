import {readFile,writeFile,lstat} from 'node:fs/promises';
import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {join,dirname} from 'node:path';

export async function packRelease(directory,version,arch,names) {
 if(!/^\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?$/.test(version)||!['amd64','arm64'].includes(arch)||!names.length||new Set(names).size!==names.length)throw new Error('Invalid release inputs');
 for(const name of names){if(!/^[a-zA-Z0-9][a-zA-Z0-9.-]*$/.test(name))throw new Error('Unsafe archive entry');const info=await lstat(join(directory,name));if(!info.isFile()||info.isSymbolicLink())throw new Error('Only regular files may be packaged')}
 const name=`reladock-v${version}-linux-${arch}.tar.gz`;const path=join(dirname(directory),name);
 const result=spawnSync('tar',['--format=ustar','-czf',path,'-C',directory,...names],{stdio:'inherit',env:{...process.env,COPYFILE_DISABLE:'1'}});if(result.status!==0)throw new Error('Release packaging failed');
 const sha256=createHash('sha256').update(await readFile(path)).digest('hex');await writeFile(`${path}.sha256`,`${sha256}  ${name}\n`);
 return {name,path,sha256};
}
