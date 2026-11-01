// Installation credentials stay in this page's memory, never localStorage.
export function bindBootstrapDialog({dialog,closeButton,onClose}) {
 const close=()=>{if(dialog.open)dialog.close();else onClose()};
 closeButton.addEventListener('click',close);
 dialog.addEventListener('close',onClose);
 return {open(){if(!dialog.open)dialog.showModal()},close};
}

export function createBootstrapSession({api,copy,now=Date.now}) {
 let grant=null;let generation=0;
 const current=()=>{if(grant&&Date.parse(grant.expiresAt)<=now())grant=null;return grant};
 return {
  current, clear(){generation++;grant=null},
  async generate(node){const ticket=++generation;grant=null;const value=await api(`/api/admin/nodes/${encodeURIComponent(node.id)}/bootstrap`,{revision:node.revision});if(ticket!==generation)return null;grant={...value,nodeId:node.id};return current()},
  async revoke(node){generation++;grant=null;await api(`/api/admin/nodes/${encodeURIComponent(node.id)}/bootstrap-revoke`,{revision:node.revision})},
  async copy(){const value=current();if(!value)throw new Error('命令已过期或已清除，请重新生成');try{await copy(value.command)}catch{throw new Error('复制失败，请手动选择命令复制')}}
 };
}

export function bindNodeBootstrap({api,getNode,reload,message}) {
 const $=s=>document.querySelector(s);
 const panel=document.createElement('dialog');panel.id='bootstrap-panel';panel.className='node-dialog bootstrap-dialog';panel.setAttribute('aria-labelledby','bootstrap-title');
 const card=document.createElement('div');card.className='node-dialog-card';
 const heading=document.createElement('div');heading.className='dialog-heading';
 const context=document.createElement('p');context.className='context-label';context.textContent='节点部署';
 const title=document.createElement('h2');title.id='bootstrap-title';title.textContent='部署入口 Agent';heading.append(context,title);
 const hint=document.createElement('p');hint.className='hint';hint.textContent='在对应的专用 Linux VPS 执行。命令有效 15 分钟，仅可注册一个节点身份，请勿转发；终端历史和剪贴板可能保留凭证。安装仅启动连接校验，不启用转发。';
 const command=document.createElement('textarea');command.rows=6;command.readOnly=true;command.setAttribute('aria-label','节点专属安装命令');
 const status=document.createElement('p');status.setAttribute('role','status');
 const output=document.createElement('pre');output.style.whiteSpace='pre-wrap';
 const controls=document.createElement('div');controls.className='inline-actions';
 const session=createBootstrapSession({api,copy:text=>navigator.clipboard.writeText(text)});let nodeID='';let viewGeneration=0;
 function action(text,fn){const b=document.createElement('button');b.type='button';b.className='secondary';b.textContent=text;b.onclick=async()=>{b.disabled=true;try{await fn()}catch(e){status.textContent=e.message}finally{b.disabled=false}};controls.append(b);return b}
 const copyButton=action('复制安装命令',()=>session.copy());
 action('重新生成',async()=>{await generate(getNode(nodeID))});
 action('撤销安装凭证',async()=>{viewGeneration++;command.value='';await session.revoke(getNode(nodeID));status.textContent='安装凭证已撤销；已建立的节点身份不会因此撤销。';await reload()});
 async function inspect(){const ticket=viewGeneration;const id=nodeID;const value=await api(`/api/admin/nodes/${encodeURIComponent(id)}/preflight`);if(ticket!==viewGeneration||id!==nodeID)return;output.textContent=value.reportedAt?`最近环境上报：${new Date(value.reportedAt).toLocaleString()}\n${JSON.stringify(value.report,null,2)}\n此报告不是转发健康证明。`:'尚无 Agent 环境上报；执行安装命令后点击刷新。'}
 action('刷新连接与检查结果',async()=>{await reload();await inspect()});
 const closeButton=document.createElement('button');closeButton.type='button';closeButton.className='secondary';closeButton.textContent='收起并清除命令';controls.append(closeButton);
 function clearView(){viewGeneration++;session.clear();nodeID='';command.value='';status.textContent='';output.textContent=''}
 const dialogView=bindBootstrapDialog({dialog:panel,closeButton,onClose:clearView});
 card.append(heading,hint,command,status,controls,output);panel.append(card);$('#node-list-panel').after(panel);
 async function generate(node){if(!node)throw new Error('请刷新节点列表');const ticket=++viewGeneration;nodeID=node.id;command.value='';status.textContent='正在生成节点专属安装命令…';output.textContent='';const g=await session.generate(node);if(ticket!==viewGeneration||!g||g.nodeId!==nodeID)return false;command.value=g.command;status.textContent=`${node.name} · 有效至 ${new Date(g.expiresAt).toLocaleString()} · 仅显示本次命令`;try{await reload()}catch{if(ticket===viewGeneration)message('安装命令已生成，但列表刷新失败，请稍后刷新。')}return ticket===viewGeneration}
 const timer=setInterval(()=>{if(!session.current()){command.value='';copyButton.disabled=true}else{copyButton.disabled=false}},1000);
 window.addEventListener('pagehide',()=>{clearInterval(timer);session.clear();command.value=''});
 return {async open(node){title.textContent=`部署入口 Agent · ${node.name}`;dialogView.open();try{if(await generate(node))await inspect()}catch(e){if(node?.id===nodeID)status.textContent=e.message}},clear(){dialogView.close()}};
}
