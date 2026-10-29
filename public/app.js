import { submitRule } from './rule-submission.js';
import { bindAccountTools,refreshAccountTools,clearCredential } from './account-tools.js';
import { bindBusinessTools,refreshBusinessTools } from './business-tools.js';
import { bindOperations } from './operations-tools.js';
const $ = selector => document.querySelector(selector);
let mode = 'login';
let identity;
const ruleDraft = {};

async function api(path, { method = 'GET', body, key } = {}) {
  const headers = {};
  if (method !== 'GET') headers['X-Requested-With'] = 'RelaDock';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (key) headers['Idempotency-Key'] = key;
  const response = await fetch(path, { method, headers, credentials: 'same-origin', body: body === undefined ? undefined : JSON.stringify(body) });
  const data = await response.json();
  if (!response.ok) {
    if (response.status === 401) showAuth();
    const error = new Error(data.error || '操作失败，请重试'); error.status = response.status; throw error;
  }
  return data;
}
function showAuth() { clearCredential();identity = undefined; ruleDraft.pending = undefined; $('#auth').hidden = false; $('#console').hidden = true; for (const id of ['message','rules-message','refresh-message','account-tools-message']) $(`#${id}`).textContent = ''; }
function setMode(value) {
  mode = value;
  $('#company-field').hidden = value !== 'register'; $('#company-field input').required = value === 'register';
  $('#auth-form [name=password]').minLength = value === 'register' ? 12 : 1;
  $('#auth-form [name=password]').autocomplete = value === 'register' ? 'new-password' : 'current-password';
  $('#auth-title').textContent = value === 'register' ? '开通企业账户' : '登录企业控制台';
  $('#auth-submit').textContent = value === 'register' ? '创建企业并进入控制台' : '登录';
  $('#login-tab').setAttribute('aria-selected', String(value === 'login')); $('#register-tab').setAttribute('aria-selected', String(value === 'register'));
  $('#auth-message').textContent = value === 'register' ? '密码需要 12–128 个字节，建议使用较长的字母、数字组合。' : '';
}
$('#login-tab').addEventListener('click', () => setMode('login'));
$('#register-tab').addEventListener('click', () => setMode('register'));
$('#auth-form').addEventListener('submit', async event => {
  event.preventDefault(); const button = $('#auth-submit'); button.disabled = true;
  const fields = Object.fromEntries(new FormData(event.target)); if (mode === 'login') delete fields.company;
  try { identity = await api(`/api/auth/${mode}`, { method: 'POST', body: fields }); event.target.reset(); $('#auth-message').textContent = ''; if (identity.platformAdmin) { location.assign('/admin'); return; } await refresh(); }
  catch (error) { $('#auth-message').textContent = error.message; } finally { button.disabled = false; }
});
$('#logout').addEventListener('click', async () => {
  $('#logout').disabled = true;
  try { await api('/api/auth/logout', { method: 'POST' }); ruleDraft.pending = undefined; showAuth(); }
  catch (error) { $('#rules-message').textContent = error.message; } finally { $('#logout').disabled = false; }
});
function cell(row, value) { row.insertCell().textContent = value; }
const actions = { 'account.register': '开通企业', 'account.login': '登录', 'account.logout': '退出登录', 'rule.create': '创建规则', 'rule.pause': '暂停配置', 'rule.resume': '恢复配置', 'rule.delete': '删除配置' };
const deliveryStates = { queued: '等待领取', leased: '领取处理中', applying: '应用中 · 等待确认', applied: '已应用 · 健康未验证', validated: '校验通过 · 未应用', failed: '配置失败 / 待重试', superseded: '已被新版本替代' };
async function refresh() {
  const data = await api('/api/overview'); identity = data.account;
  $('#auth').hidden = true; $('#console').hidden = false; $('#company-name').textContent = identity.company; $('#account-email').textContent = identity.email;
  $('#platform-link').hidden = !identity.platformAdmin;
  const editable = ['owner', 'admin'].includes(identity.role); $('#rule-submit').disabled = !editable;
  const nodeRoot = $('#nodes'); nodeRoot.replaceChildren();
  const selector = $('#node-select'); const selected = selector.value; selector.replaceChildren();
  for (const node of data.nodes) {
    const label = node.status === 'maintenance' ? '维护中' : node.appliedGeneration > 0 ? '已有应用确认 · 健康未知' : '尚未部署';
    const option = document.createElement('option'); option.value = node.id; option.textContent = `${node.name} · ${label}`; option.disabled = node.status === 'maintenance'; selector.append(option);
    const row = nodeRoot.insertRow();
    const delivery = node.deliveryState === 'failed' && node.deliveryAttempts >= 5 ? '重试已耗尽 · 待运维处理' : deliveryStates[node.deliveryState] || '未知';
    for (const value of [node.name, node.region, label, `${node.desiredGeneration} / ${node.validatedGeneration} / ${node.appliedGeneration}`, delivery, node.lastSeenAt ? `${new Date(node.lastSeenAt).toLocaleString()}（控制通道）` : '尚无联系记录']) cell(row, value);
  }
  if ([...selector.options].some(option => option.value === selected && !option.disabled)) selector.value = selected;
  else { const available = [...selector.options].find(option => !option.disabled); selector.value = available?.value || ''; }
  $('#rule-submit').disabled = !editable || !selector.value;
  $('#refresh-message').textContent = `最近刷新：${new Date().toLocaleTimeString()}。应用确认不等于端到端健康。`;
  const root = $('#rules'); root.replaceChildren(); $('#rule-count').textContent = `(${data.rules.length})`;
  if (!data.rules.length) { const c = root.insertRow().insertCell(); c.colSpan = 9; c.className = 'table-empty'; c.textContent = '暂无规则，请填写客户的目标服务器。'; }
  for (const rule of data.rules) {
    const row = root.insertRow(); const target = rule.targetHost.includes(':') ? `[${rule.targetHost}]:${rule.targetPort}` : `${rule.targetHost}:${rule.targetPort}`;
    for (const value of [`${rule.name} / ${rule.nodeId}`, rule.listenPort, rule.protocol.toUpperCase(), target,`${rule.bandwidthMbps} Mbps`]) cell(row, value);
    cell(row,'未收到采集');cell(row,`期望 ${rule.desiredVersion} / 生效 ${rule.appliedVersion}`);
    const usageCell=row.cells[5];
    const usageButton=document.createElement('button');usageButton.className='secondary';usageButton.textContent='查询本规则';usageButton.addEventListener('click',async()=>{usageButton.disabled=true;try{const u=await api(`/api/billing/usage?ruleId=${encodeURIComponent(rule.id)}`);usageCell.textContent=u.hasReports?`上行 ${u.uploadBytes.toLocaleString()} B / 下行 ${u.downloadBytes.toLocaleString()} B`:'未收到采集报告';}catch(e){usageCell.textContent=e.message;}});usageCell.append(usageButton);
    const badge = document.createElement('span'); badge.className = 'badge'; badge.textContent = rule.appliedVersion !== rule.desiredVersion ? '等待应用确认' : rule.appliedStatus === 'paused' ? '已应用暂停配置' : '已应用 · 健康未知'; if (!rule.appliedVersion) badge.textContent = rule.status === 'paused' ? '待确认暂停' : '待 Agent 部署'; row.insertCell().append(badge);
    const probeButton = document.createElement('button'); probeButton.className = 'secondary'; probeButton.textContent = '目标可达性';
    probeButton.addEventListener('click', async () => {
      probeButton.disabled = true;
      try {
        const observations = await api(`/api/operations/probes?ruleId=${encodeURIComponent(rule.id)}`);
        const labels = { targetReachable: '目标 TCP 可连接', targetUnreachable: '目标 TCP 连接失败', unknown: 'UDP 健康未知' };
        probeButton.textContent = observations.length ? observations.map(item => `${labels[item.status] || '未知'} · ${new Date(item.observedAt).toLocaleString()}`).join('；') : '尚无当前配置观测';
        probeButton.title = '仅从入口节点观测目标，不代表客户端到端转发健康。';
      } catch (error) { probeButton.textContent = error.message; }
      finally { probeButton.disabled = false; }
    });
    row.cells[7].append(probeButton);
    const controls = row.insertCell(); controls.className = 'table-actions';
    if (editable) { controls.append(actionButton(rule.status === 'paused' ? '恢复' : '暂停', rule.id, rule.status === 'paused' ? 'resume' : 'pause')); controls.append(actionButton('删除', rule.id, 'delete')); }
  }
  const audit = $('#audit-rows'); audit.replaceChildren();
  for (const entry of data.auditLogs) { const row = audit.insertRow(); cell(row, new Date(entry.createdAt).toLocaleString()); cell(row, actions[entry.action] || entry.action); cell(row, entry.resourceId); }
  try{await refreshAccountTools(api,identity);}catch(e){$('#account-tools-message').textContent=e.message;}
  try{await refreshBusinessTools(api,identity,data.rules);}catch(e){$('#business-message').textContent=e.message;}
}
function actionButton(label, id, action) {
  const button = document.createElement('button'); button.textContent = label; button.className = `secondary ${action === 'delete' ? 'danger' : ''}`;
  button.addEventListener('click', async () => {
    if (action === 'delete' && !window.confirm('删除此规则？已下发的端口将保留隔离，等待移除确认与连接跟踪核对，不立即复用。')) return;
    button.disabled = true;
    try { await api(`/api/rules/${id}/${action}`, { method: 'POST' }); $('#rules-message').textContent = action === 'delete' ? '删除请求已记录；已下发端口保持隔离，未下发端口可释放。' : action === 'resume' ? '恢复请求已进入交付队列，等待确认。' : '暂停请求已进入交付队列，端口仍保留。'; await refresh(); }
    catch (error) { $('#rules-message').textContent = error.message; button.disabled = false; }
  }); return button;
}
$('#rule-form').addEventListener('submit', async event => {
  event.preventDefault(); const button = $('#rule-submit'); button.disabled = true;
  const fields = Object.fromEntries(new FormData(event.target)); fields.targetPort = Number(fields.targetPort); fields.bandwidthMbps = Number(fields.bandwidthMbps);
  try {
    const { rule, refreshError } = await submitRule(ruleDraft, fields, (body, key) => api('/api/rules', { method: 'POST', body, key }), refresh);
    $('#message').textContent = `已预留 ${rule.protocol.toUpperCase()} 端口 ${rule.listenPort}。配置等待 Agent 交付，尚未转发流量。${refreshError ? ' 配置已保存，但列表刷新失败；可重试，原请求不会重复创建。' : ''}`;
  }
  catch (error) { $('#message').textContent = error.message; } finally { button.disabled = !identity || !['owner', 'admin'].includes(identity.role) || !$('#node-select').value; }
});
setMode('login');
bindAccountTools(api,refresh);
bindBusinessTools(api,refresh);
bindOperations(api,{canWrite:()=>identity&&['owner','admin'].includes(identity.role)});
$('#refresh').addEventListener('click', async () => { $('#refresh').disabled = true; try { await refresh(); } catch (error) { $('#refresh-message').textContent = error.message; } finally { $('#refresh').disabled = false; } });
refresh().catch(error => { showAuth(); if (error.status !== 401) $('#auth-message').textContent = error.message; });
