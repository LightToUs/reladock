import { bindOperations } from './operations-tools.js';
const $ = selector => document.querySelector(selector);
const state = { view: 'overview', nodes: [], editingNode: null, certificateNode: null, tenantPage: 1, rulePage: 1, tenantFilter: '', tenantName: '', nodeFilter: '' };
const titles = { overview: '平台总览', nodes: '入口节点', tenants: '企业客户', rules: '全局规则', deliveries: '配置交付', plans:'规则套餐',operations:'告警工单',costs:'成本与经营差额',audit: '操作审计' };
const deliveries = { queued: '等待领取', leased: '领取处理中', applying: '应用中 · 等待确认', applied: '已应用 · 健康未知', validated: '校验通过 · 未应用', failed: '配置失败', superseded: '已被新版本替代' };
const actions = { 'node.add': '新增节点', 'node.edit': '编辑节点', 'node.maintenance': '进入维护', 'node.enable': '恢复分配', 'node.enroll': '绑定 / 轮换证书', 'node.revoke-certificate': '撤销证书', 'config.retry': '重新交付', 'tenant.access': '修改企业登录状态', 'platform.admin-access': '修改平台权限', 'rule.pause': '暂停规则', 'rule.resume': '恢复规则', 'rule.delete': '删除规则' };
const costNav = document.createElement('button'); costNav.dataset.view = 'costs'; costNav.textContent = '成本与经营差额'; document.querySelector('.admin-nav nav').append(costNav);
const notificationNav = document.createElement('button'); notificationNav.dataset.view = 'notifications'; notificationNav.textContent = '通知交付'; document.querySelector('.admin-nav nav').append(notificationNav); titles.notifications = '通知交付';
function access(mode) { $('#admin-login').hidden = mode !== 'login'; $('#admin-denied').hidden = mode !== 'denied'; $('#admin-shell').hidden = mode !== 'admin'; }
function message(text) { $('#admin-message').textContent = text; }
async function api(path, body, key) {
  const options = { credentials: 'same-origin', headers: {} };
  if (body !== undefined) { options.method = 'POST'; options.headers = { 'Content-Type': 'application/json', 'X-Requested-With': 'RelaDock' }; options.body = JSON.stringify(body); }
  if (key) options.headers['Idempotency-Key'] = key;
  const response = await fetch(path, options); const data = await response.json();
  if (!response.ok) { if (response.status === 401) access('login'); if (response.status === 403) access('denied'); const error = new Error(data.error || '操作失败'); error.status = response.status; throw error; }
  return data;
}
function cell(row, value, detail) { const td = row.insertCell(); td.textContent = value; if (detail) { const span = document.createElement('span'); span.className = 'detail'; span.textContent = detail; td.append(span); } return td; }
function empty(root, span, text) { if (!root.children.length) { const td = root.insertRow().insertCell(); td.colSpan = span; td.className = 'table-empty'; td.textContent = text; } }
function button(label, task, danger = false) { const el = document.createElement('button'); el.type = 'button'; el.textContent = label; el.className = `secondary${danger ? ' danger' : ''}`; el.addEventListener('click', async () => { el.disabled = true; try { await task(); } catch (error) { message(error.message); } finally { el.disabled = false; } }); return el; }
function showView(view) { state.view = view; for (const pane of document.querySelectorAll('[data-pane]')) pane.hidden = pane.dataset.pane !== view; for (const nav of document.querySelectorAll('[data-view]')) nav.classList.toggle('selected', nav.dataset.view === view); $('#view-title').textContent = titles[view]; if (view === 'deliveries') loadDeliveries().catch(error => message(error.message)); if(view==='plans')loadPlans().catch(e=>message(e.message));if(view==='operations')operations.refresh().catch(e=>message(e.message)); }
for (const nav of document.querySelectorAll('[data-view]')) nav.addEventListener('click', () => showView(nav.dataset.view));
costNav.addEventListener('click', () => loadCosts().catch(error => message(error.message)));
notificationNav.addEventListener('click', () => loadNotifications().catch(error => message(error.message)));
for (const nav of document.querySelectorAll('[data-jump]')) nav.addEventListener('click', () => showView(nav.dataset.jump));

async function reload() {
  const paths = ['/api/admin/overview', '/api/admin/nodes', `/api/admin/tenants?page=${state.tenantPage}`, `/api/admin/rules?page=${state.rulePage}&tenantId=${encodeURIComponent(state.tenantFilter)}&nodeId=${encodeURIComponent(state.nodeFilter)}`, '/api/admin/audit'];
  const results = await Promise.allSettled(paths.map(path => api(path)));
  const errors = results.filter(result => result.status === 'rejected'); if (errors.length) throw errors[0].reason;
  const [summary, nodes, tenants, rules, audit] = results.map(result => result.value);
  state.nodes = nodes; renderSummary(summary); renderNodes(nodes); renderTenants(tenants); renderRules(rules); renderAudit(audit);
  const costNode = $('#cost-node'); const savedCostNode = costNode.value; costNode.replaceChildren();
  for (const node of nodes) { const option = document.createElement('option'); option.value = node.id; option.textContent = node.name; costNode.append(option); }
  if ([...costNode.options].some(option => option.value === savedCostNode)) costNode.value = savedCostNode;
  for (const selector of [$('#delivery-node'), $('#rules-node')]) { const selected = selector.value; selector.replaceChildren(); if (selector.id === 'rules-node') { const all = document.createElement('option'); all.value = ''; all.textContent = '全部节点'; selector.append(all); } for (const node of nodes) { const option = document.createElement('option'); option.value = node.id; option.textContent = node.name; selector.append(option); } if ([...selector.options].some(option => option.value === selected)) selector.value = selected; }
  $('#rules-node').value = state.nodeFilter;
  if (state.view === 'deliveries') await loadDeliveries();
}
function renderSummary(data) { $('#stat-tenants').textContent = data.tenants; $('#stat-suspended').textContent = `${data.suspendedTenants} 家已停用登录`; $('#stat-nodes').textContent = data.nodes; $('#stat-maintenance').textContent = `${data.maintenanceNodes} 个维护中`; $('#stat-rules').textContent = data.rules; $('#stat-paused').textContent = `${data.pausedRules} 条已暂停`; $('#stat-failed').textContent = data.failedDeliveries; }
async function mutate(path, body, success) {
  try { await api(path, body); }
  catch (error) { if (error.status === 409) { await reload(); const nodeId = /^\/api\/admin\/nodes\/([^/]+)\//.exec(path)?.[1]; const latest = state.nodes.find(node => node.id === nodeId); if (latest && state.editingNode?.id === nodeId) editNode(latest); if (latest && state.certificateNode?.id === nodeId) editCertificate(latest); message('操作冲突，已载入最新数据，请重新确认后操作。有规则的节点不可更换绑定地址或降低至规则速率以下。'); return false; } throw error; }
  message(success);
  try { await reload(); } catch (error) { message(`${success} 列表刷新失败，请点击刷新查看结果：${error.message}`); }
  return true;
}
function renderNodes(nodes) {
  const root = $('#admin-nodes'); root.replaceChildren();
  for (const node of nodes) { const row = root.insertRow(); cell(row, node.name, node.id); cell(row, node.region, node.publicAddress || '计划公网 IP 未填写'); cell(row, `${node.capacityMbps} Mbps`); cell(row, node.status === 'maintenance' ? '维护中' : '尚未部署'); cell(row, `${node.desiredGeneration} / ${node.validatedGeneration}`, `修订 ${node.revision}`); cell(row, node.deliveryState === 'failed' && node.deliveryAttempts >= 5 ? '重试耗尽 · 待处理' : deliveries[node.deliveryState] || '未知', node.lastSeenAt ? `最近联系 ${new Date(node.lastSeenAt).toLocaleString()}` : '尚无联系记录'); cell(row, node.certificateFingerprint ? '已绑定' : '未绑定'); const controls = row.insertCell(); controls.className = 'table-actions'; controls.append(button('编辑', () => editNode(node)), button(node.status === 'maintenance' ? '恢复分配' : '进入维护', () => mutate(`/api/admin/nodes/${node.id}/${node.status === 'maintenance' ? 'enable' : 'maintenance'}`, { revision: node.revision }, '节点分配状态已更新。')), button('证书', () => editCertificate(node)), button('重试交付', () => mutate(`/api/admin/nodes/${node.id}/retry`, { revision: node.revision }, '已生成最新完整快照，等待 Agent 领取。')), button('交付记录', () => { $('#delivery-node').value = node.id; showView('deliveries'); })); }
  empty(root, 8, '暂无节点，请先登记入口资源。');
  for (let i = 0; i < nodes.length; i++) {
    const node = nodes[i]; const row = root.rows[i];
    row.cells[3].textContent = node.status === 'maintenance' ? '维护中' : node.appliedGeneration > 0 ? '已有应用确认 · 健康未知' : '尚未部署';
    row.cells[4].replaceChildren();
    row.cells[4].append(document.createTextNode(`${node.desiredGeneration} / ${node.validatedGeneration} / ${node.appliedGeneration}（修订 ${node.revision}）`));
  }
}
function editNode(node) { state.editingNode = node; const form = $('#node-form'); for (const name of ['id', 'name', 'region', 'listenAddress', 'publicAddress', 'capacityMbps']) form.elements.namedItem(name).value = node[name]; form.elements.namedItem('id').readOnly = true; $('#node-form-title').textContent = `编辑节点 · ${node.name}`; $('#node-save').textContent = '保存节点修改'; $('#node-form-title').scrollIntoView({ behavior: 'smooth', block: 'start' }); }
$('#node-new').addEventListener('click', () => { state.editingNode = null; $('#node-form').reset(); $('#node-form [name=id]').readOnly = false; $('#node-form-title').textContent = '新增入口节点'; $('#node-save').textContent = '登记节点'; });
$('#node-form').addEventListener('submit', async event => { event.preventDefault(); const submit = $('#node-save'); submit.disabled = true; const input = Object.fromEntries(new FormData(event.target)); input.capacityMbps = Number(input.capacityMbps); const editing = state.editingNode; if (editing) input.revision = editing.revision; try { const saved = await mutate(editing ? `/api/admin/nodes/${editing.id}/update` : '/api/admin/nodes', input, editing ? '节点资料已更新。' : '节点已登记，尚未部署实际转发。'); if (saved) $('#node-new').click(); } catch (error) { message(error.message); } finally { submit.disabled = false; } });
function editCertificate(node) { state.certificateNode = node; $('#certificate-panel').hidden = false; $('#certificate-node').textContent = node.name; $('#certificate-fingerprint').textContent = node.certificateFingerprint || '未绑定'; $('#certificate-revoke').disabled = !node.certificateFingerprint; $('#certificate-form').reset(); $('#certificate-panel').scrollIntoView({ behavior: 'smooth', block: 'start' }); }
$('#certificate-close').addEventListener('click', () => { $('#certificate-panel').hidden = true; state.certificateNode = null; });
$('#certificate-form').addEventListener('submit', async event => { event.preventDefault(); const node = state.certificateNode; if (!node) return; const submit = event.target.querySelector('button'); submit.disabled = true; try { const saved = await mutate(`/api/admin/nodes/${node.id}/enroll`, { revision: node.revision, certificate: event.target.elements.namedItem('certificate').value }, '证书已绑定，旧授权已失效，最新配置已重新入队。'); if (saved) $('#certificate-close').click(); } catch (error) { message(error.message); } finally { submit.disabled = false; } });
$('#certificate-revoke').addEventListener('click', async () => { const node = state.certificateNode; if (!node || !confirm('撤销这个节点当前证书的 Agent 接入权限？不会停止已有数据面流量。')) return; $('#certificate-revoke').disabled = true; try { if (await mutate(`/api/admin/nodes/${node.id}/revoke-certificate`, { revision: node.revision }, '节点证书授权已撤销。')) $('#certificate-close').click(); } catch (error) { message(error.message); $('#certificate-revoke').disabled = false; } });

function pager(kind, page) { $(`#${kind}-page`).textContent = `第 ${page.page} 页 · 共 ${page.total} 条`; $(`#${kind}-prev`).disabled = page.page <= 1; $(`#${kind}-next`).disabled = page.page * page.pageSize >= page.total; }
function renderTenants(page) { const root = $('#admin-tenants'); root.replaceChildren(); for (const tenant of page.items) { const row = root.insertRow(); cell(row, tenant.name, tenant.ownerEmail); cell(row, tenant.members); cell(row, tenant.rules); cell(row, tenant.status === 'active' ? '允许登录' : '已停用登录', tenant.hasPlatformAdmin ? '含平台管理员账户' : ''); const controls = row.insertCell(); controls.className = 'table-actions'; controls.append(button('查看规则', async () => { state.tenantFilter = tenant.id; state.tenantName = tenant.name; state.rulePage = 1; await reload(); showView('rules'); })); if (!tenant.hasPlatformAdmin) controls.append(button(tenant.status === 'active' ? '停用登录' : '恢复登录', async () => { if (tenant.status === 'active' && !confirm(`停用“${tenant.name}”登录并撤销其全部会话？已有转发流量不会自动停止。`)) return; await mutate(`/api/admin/tenants/${tenant.id}/access`, { enabled: tenant.status !== 'active', revision: tenant.revision }, '企业登录权限已更新，原会话已撤销。'); }, tenant.status === 'active')); } empty(root, 5, '本页暂无企业。'); pager('tenants', page); }
function renderRules(page) {
  const root = $('#admin-rules'); root.replaceChildren(); $('#rules-tenant-label').textContent = state.tenantFilter ? `企业：${state.tenantName}` : '全部企业';
  for (const rule of page.items) {
    const row = root.insertRow(); cell(row, rule.company, rule.name); cell(row, rule.nodeId, `端口 ${rule.listenPort}`); cell(row, rule.protocol.toUpperCase()); cell(row, rule.targetHost.includes(':') ? `[${rule.targetHost}]:${rule.targetPort}` : `${rule.targetHost}:${rule.targetPort}`); cell(row, `${rule.bandwidthMbps} Mbps`); cell(row, `${rule.desiredVersion} / ${rule.appliedVersion}`);
    const applied = rule.appliedVersion > 0 && rule.appliedVersion === rule.desiredVersion;
    cell(row, applied ? (rule.appliedStatus === 'paused' ? '已应用暂停配置' : '已应用 · 健康未知') : '等待应用确认');
    const controls = row.insertCell(); controls.className = 'table-actions';
    for (const action of [rule.status === 'paused' ? 'resume' : 'pause', 'delete']) controls.append(button({ resume: '恢复', pause: '暂停', delete: '删除' }[action], async () => {
      if (action === 'delete' && !confirm(`删除“${rule.company} / ${rule.name}”？已下发端口保持隔离，不立即复用。`)) return;
      await mutate(`/api/admin/rules/${rule.id}/${action}`, { tenantId: rule.tenantId, version: rule.desiredVersion }, '规则请求已入队，等待 Agent 确认；平台审计已记录。');
    }, action === 'delete'));
  }
  empty(root, 8, '本页暂无匹配规则。'); pager('rules', page);
}
function renderAudit(records) { const root = $('#admin-audit'); root.replaceChildren(); for (const log of records) { const row = root.insertRow(); cell(row, new Date(log.createdAt).toLocaleString()); cell(row, log.actor === 'local-operator' ? '本机运维' : log.actor); cell(row, actions[log.action] || log.action); cell(row, log.resourceId).className = 'audit-resource'; cell(row, Object.entries(log.details).map(([key, value]) => `${key}: ${String(value)}`).join(' · ') || '—').className = 'audit-detail'; } empty(root, 5, '暂无平台操作记录。'); }
async function loadDeliveries() { const root = $('#admin-deliveries'); const id = $('#delivery-node').value; if (!id) { root.replaceChildren(); empty(root, 6, '请先登记节点。'); return; } const records = await api(`/api/admin/nodes/${id}/deliveries`); root.replaceChildren(); for (const record of records) { const row = root.insertRow(); for (const value of [record.generation, deliveries[record.state] || record.state, record.attempts, ({ 'invalid-config': '配置检查失败', 'internal-error': 'Agent 内部错误', 'lease-exhausted': '租约重试耗尽' })[record.errorCode] || record.errorCode || '—', new Date(record.createdAt).toLocaleString(), record.reportedAt ? new Date(record.reportedAt).toLocaleString() : '尚未回报']) cell(row, value); } empty(root, 6, '暂无交付记录。'); }
$('#delivery-node').addEventListener('change', () => loadDeliveries().catch(error => message(error.message)));
$('#rule-filters').addEventListener('submit', async event => { event.preventDefault(); state.nodeFilter = $('#rules-node').value; state.rulePage = 1; try { await reload(); } catch (error) { message(error.message); } });
$('#rules-clear-tenant').addEventListener('click', async () => { state.tenantFilter = ''; state.tenantName = ''; state.rulePage = 1; try { await reload(); } catch (error) { message(error.message); } });
for (const kind of ['tenants', 'rules']) for (const direction of ['prev', 'next']) $(`#${kind}-${direction}`).addEventListener('click', async () => { const key = kind === 'tenants' ? 'tenantPage' : 'rulePage'; state[key] += direction === 'next' ? 1 : -1; try { await reload(); } catch (error) { message(error.message); } });
async function enter() { const who = await api('/api/auth/me'); if (!who.platformAdmin) { access('denied'); return; } $('#admin-email').textContent = who.email; access('admin'); await reload(); }
$('#admin-login-form').addEventListener('submit', async event => { event.preventDefault(); const submit = event.target.querySelector('button'); submit.disabled = true; try { await api('/api/auth/login', Object.fromEntries(new FormData(event.target))); event.target.reset(); $('#login-message').textContent = ''; await enter(); } catch (error) { $('#login-message').textContent = error.message; } finally { submit.disabled = false; } });
async function logout() { try { await api('/api/auth/logout', {}); location.assign('/admin'); } catch (error) { message(error.message); } }
$('#admin-logout').addEventListener('click', logout); $('#denied-logout').addEventListener('click', logout);
$('#admin-refresh').addEventListener('click', async () => { $('#admin-refresh').disabled = true; try { await reload(); message('已刷新最新平台数据。'); } catch (error) { message(error.message); } finally { $('#admin-refresh').disabled = false; } });
async function loadPlans(){const data=await api('/api/admin/plans');const root=$('#plan-rows');root.replaceChildren();for(const p of data.items){const row=root.insertRow();for(const v of [p.name,`${p.code} / ${p.version}`,`$${(p.priceCents/100).toFixed(2)}`,`${(p.quotaBytes/1e9).toLocaleString()} GB`,`${p.bandwidthMbps} Mbps`,`${p.periodSeconds/86400} 天`])cell(row,v);}empty(root,6,'尚未配置套餐。');}
$('#plan-form').addEventListener('submit',async e=>{e.preventDefault();const b=e.target.querySelector('button');b.disabled=true;try{const fields=Object.fromEntries(new FormData(e.target));const input={code:fields.code,name:fields.name,version:Number(fields.version),priceCents:Number(fields.priceCents),quotaBytes:Number(fields.quotaGB)*1e9,bandwidthMbps:Number(fields.bandwidthMbps),periodSeconds:Number(fields.periodDays)*86400};await api('/api/admin/plans',input);e.target.reset();await loadPlans();message('已创建不可变价格版本。');}catch(err){message(err.message);}finally{b.disabled=false;}});
const operations=bindOperations((path,{body}={})=>api(path,body),{admin:true});
let costPage = 1; let costDraft;
let notificationPage = 1;
async function loadNotifications() {
  const data = await api(`/api/admin/notifications?page=${notificationPage}`); const root = $('#notification-rows'); root.replaceChildren();
  const labels = { pending: '等待发送', claimed: '交付中', delivered: '接收端已确认', exhausted: '重试耗尽' };
  for (const item of data.items) {
    const row = root.insertRow(); cell(row, item.nodeId, item.kind); cell(row, item.incidentState === 'open' ? '告警发生' : '告警解除'); cell(row, labels[item.state] || item.state, item.errorCode); cell(row, item.attempts); cell(row, item.deliveredAt ? new Date(item.deliveredAt).toLocaleString() : '未确认');
    const controls = row.insertCell(); if (item.state === 'exhausted') controls.append(button('重新交付', async () => { await api('/api/admin/notifications/retry', { eventId: item.eventId }); await loadNotifications(); message('事件已重新入队；接收端仍按原事件编号去重。'); }));
  }
  empty(root, 6, '暂无真实告警通知事件。'); $('#notification-page').textContent = `第 ${notificationPage} 页 · 共 ${data.total} 条`; $('#notification-prev').disabled = notificationPage <= 1; $('#notification-next').disabled = notificationPage * data.pageSize >= data.total;
}
$('#notification-refresh').addEventListener('click', () => loadNotifications().catch(error => message(error.message)));
for (const direction of ['prev', 'next']) $(`#notification-${direction}`).addEventListener('click', () => { notificationPage += direction === 'next' ? 1 : -1; loadNotifications().catch(error => message(error.message)); });
const costFrom = $('#cost-query [name=from]'); const costTo = $('#cost-query [name=to]');
costFrom.value = new Date(Date.now() - 30 * 86400000).toISOString().slice(0, 10); costTo.value = new Date(Date.now() + 86400000).toISOString().slice(0, 10);
async function loadCosts() {
  const query = new URLSearchParams({ from: `${costFrom.value}T00:00:00Z`, to: `${costTo.value}T00:00:00Z`, page: String(costPage) });
  const [report, entries] = await Promise.all([api(`/api/admin/costs/margin?${query}`), api(`/api/admin/costs?${query}`)]);
  const show = value => value === null ? '成本未完整录入' : String(value);
  $('#cost-summary').textContent = `套餐销售 ${report.totals.revenueCents} 美分；成本 ${show(report.totals.costCents)}；经营差额 ${show(report.totals.marginCents)}。现金口径，非应计会计毛利。`;
  const margins = $('#cost-margin-rows'); margins.replaceChildren();
  for (const node of report.nodes.items) { const row = margins.insertRow(); for (const value of [node.name, node.revenueCents, show(node.costCents), show(node.marginCents)]) cell(row, value); }
  empty(margins, 4, '暂无节点记录。');
  const costs = $('#cost-entry-rows'); costs.replaceChildren();
  for (const entry of entries.items) { const row = costs.insertRow(); for (const value of [new Date(entry.recordedAt).toLocaleString(), entry.nodeId, entry.amountCents, entry.invoiceRef]) cell(row, value); }
  empty(costs, 4, '此窗口尚未录入成本。');
  $('#cost-page').textContent = `第 ${costPage} 页（每页最多 100 条）`; $('#cost-prev').disabled = costPage <= 1;
  $('#cost-next').disabled = costPage * 100 >= Math.max(report.nodes.total, entries.total);
}
$('#cost-query').addEventListener('submit', event => { event.preventDefault(); costPage = 1; loadCosts().catch(error => message(error.message)); });
for (const direction of ['prev', 'next']) $(`#cost-${direction}`).addEventListener('click', () => { costPage += direction === 'next' ? 1 : -1; loadCosts().catch(error => message(error.message)); });
$('#cost-form').addEventListener('submit', async event => {
  event.preventDefault(); const button = event.target.querySelector('button'); button.disabled = true;
  try {
    const fields = Object.fromEntries(new FormData(event.target)); const input = { ...fields, amountCents: Number(fields.amountCents), serviceFrom: new Date(fields.serviceFrom).toISOString(), serviceTo: new Date(fields.serviceTo).toISOString() };
    const serialized = JSON.stringify(input); if (costDraft?.serialized !== serialized) costDraft = { serialized, key: crypto.randomUUID() };
    await api('/api/admin/costs', input, costDraft.key); costDraft = undefined; $('#cost-message').textContent = '实际成本已录入；记录不可修改或删除，平台审计已保存。'; event.target.reset(); await loadCosts();
  } catch (error) { $('#cost-message').textContent = error.message; }
  finally { button.disabled = false; }
});
enter().catch(error => { if (error.status !== 401 && error.status !== 403) { access('login'); $('#login-message').textContent = error.message; } });
