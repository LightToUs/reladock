const $ = s => document.querySelector(s);
const roles = {owner:'企业所有者',admin:'企业管理员',viewer:'只读成员'};
function cells(row,values){for(const value of values)row.insertCell().textContent=value;}
function button(label,action){const b=document.createElement('button');b.className='secondary';b.textContent=label;b.addEventListener('click',async()=>{b.disabled=true;try{await action();}catch(e){$('#account-tools-message').textContent=e.message;}finally{b.disabled=false;}});return b;}
export function clearCredential(){ $('#credential-token').value='';$('#credential-result').hidden=true; }
function reveal(token){$('#credential-token').value=token;$('#credential-result').hidden=false;$('#credential-token').focus();$('#credential-token').select();}
export function bindAccountTools(api,refresh){
  $('#credential-clear').addEventListener('click',clearCredential);
  $('#accept-invitation').addEventListener('submit',async e=>{e.preventDefault();const b=e.target.querySelector('button');b.disabled=true;try{await api('/api/auth/invitations/accept',{method:'POST',body:Object.fromEntries(new FormData(e.target))});e.target.reset();await refresh();}catch(err){$('#accept-message').textContent=err.message;}finally{b.disabled=false;}});
  for(const [form,path,days]of [['#invitation-form','/api/invitations',1],['#api-key-form','/api/api-keys',30]]){
    $(form).addEventListener('submit',async e=>{e.preventDefault();const b=e.target.querySelector('button');b.disabled=true;clearCredential();try{const body=Object.fromEntries(new FormData(e.target));body.expiresAt=new Date(Date.now()+days*86400000).toISOString();const data=await api(path,{method:'POST',body});e.target.reset();await refresh();reveal(data.token);}catch(err){$('#account-tools-message').textContent=err.message;}finally{b.disabled=false;}});
  }
}
export async function refreshAccountTools(api,identity){
  const editable=['owner','admin'].includes(identity.role);
  $('#member-tools').hidden=!editable;
  $('#api-key-form [value=write]').disabled=!editable;
  if(!editable)$('#api-key-form [name=scope]').value='read';
  const [keys,members,invitations]=await Promise.all([api('/api/api-keys'),editable?api('/api/members'):[],editable?api('/api/invitations'):[]]);
  const update=()=>refreshAccountTools(api,identity);
  const root=$('#member-rows');root.replaceChildren();
  for(const m of members){const row=root.insertRow();cells(row,[m.email,roles[m.role]||m.role]);const controls=row.insertCell();if(m.role!=='owner'&&m.id!==identity.memberId)controls.append(button(m.role==='admin'?'改为只读':'设为管理员',async()=>{await api(`/api/members/${m.id}/role`,{method:'POST',body:{role:m.role==='admin'?'viewer':'admin'}});await update();}));}
  const invites=$('#invitation-rows');invites.replaceChildren();for(const i of invitations){const row=invites.insertRow();const active=!i.acceptedAt&&!i.revokedAt&&new Date(i.expiresAt)>new Date();cells(row,[i.email,new Date(i.expiresAt).toLocaleString(),i.acceptedAt?'已接受':i.revokedAt?'已撤销':active?'待接受':'已过期']);const c=row.insertCell();if(active)c.append(button('撤销',async()=>{await api(`/api/invitations/${i.id}/revoke`,{method:'POST',body:{}});await update();}));}
  const keyRows=$('#key-rows');keyRows.replaceChildren();for(const k of keys){const row=keyRows.insertRow();const active=!k.revokedAt&&new Date(k.expiresAt)>new Date();cells(row,[k.name,k.scope==='write'?'读写':'只读',new Date(k.expiresAt).toLocaleString(),k.revokedAt?'已撤销':active?'有效':'已过期']);const c=row.insertCell();if(active)c.append(button('撤销',async()=>{await api(`/api/api-keys/${k.id}/revoke`,{method:'POST',body:{}});await update();}));}
}
