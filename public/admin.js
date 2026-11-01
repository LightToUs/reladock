import { bindOperations } from './operations-tools.js';
import { bindNodeBootstrap } from './node-bootstrap.js';
import { bindAdminNavigation } from './admin-navigation.js';
import { agentContactPresentation, certificatePresentation, configurationPresentation, probePresentation, resourcePresentation } from './node-contact-status.js';
import { bindNodeActionDialog, createNodeActionRunner, nodeActionDescriptor } from './node-action-dialog.js';
import { bindNodeContentDialog, bindNodeDeliveryDialog, runDialogSession } from './node-content-dialog.js';
import { bindNodeEditor } from './node-editor.js';
import { createAdaptivePoller, nodeMonitoringPresentation } from './node-monitoring.js';
const $ = selector => document.querySelector(selector);
const state = { view: 'overview', nodes: [], monitoring: new Map(), certificateNode: null, tenantPage: 1, rulePage: 1, tenantFilter: '', tenantName: '', nodeFilter: '' };
let adminNodeMonitoringPoller;
const titles = { overview: '平台总览', nodes: '入口节点', tenants: '企业客户', rules: '全局规则', deliveries: '配置交付', plans:'规则套餐',operations:'告警工单',costs:'成本与经营差额',audit: '操作审计' };
const deliveries = { queued: '等待领取', leased: '领取处理中', applying: '应用中 · 等待确认', applied: '已应用 · 健康未知', validated: '校验通过 · 未应用', failed: '配置失败', superseded: '已被新版本替代' };
const upgradeStates = { queued: '等待 Agent 领取', claimed: '已领取', installing: '正在安装', verifying: '正在验证', succeeded: '升级成功', 'rolled-back': '已自动回滚', failed: '升级失败', cancelled: '已取消' };
const upgradeErrors = { 'download-unavailable': '发布包下载失败', 'archive-invalid': '发布包格式无效', 'checksum-mismatch': '发布包摘要不匹配', 'install-failed': '安装失败', 'service-failed': 'Agent 启动失败', 'health-timeout': '控制链路验证超时', 'rollback-failed': '回滚失败，需要人工处理', 'state-invalid': '升级状态不一致' };
const actions = { 'node.add': '新增节点', 'node.edit': '编辑节点', 'node.withdraw-registration': '撤销节点登记', 'node.maintenance': '进入维护', 'node.enable': '恢复分配', 'node.draining': '进入安全排空', 'node.retired': '最终退役节点', 'node.enroll': '绑定 / 轮换证书', 'node.revoke-certificate': '撤销证书', 'node.agent-upgrade-request': '创建 Agent 升级', 'node.agent-upgrade-cancel': '取消 Agent 升级', 'node.agent-upgrade-claimed': 'Agent 领取升级', 'node.agent-upgrade-installing': 'Agent 安装升级', 'node.agent-upgrade-verifying': 'Agent 验证升级', 'node.agent-upgrade-succeeded': 'Agent 升级成功', 'node.agent-upgrade-rolled-back': 'Agent 已自动回滚', 'node.agent-upgrade-failed': 'Agent 升级失败', 'config.retry': '重新交付', 'tenant.access': '修改企业登录状态', 'platform.admin-access': '修改平台权限', 'rule.pause': '暂停规则', 'rule.resume': '恢复规则', 'rule.delete': '删除规则' };
const costNav = document.createElement('button'); costNav.dataset.view = 'costs'; costNav.textContent = '成本与经营差额'; document.querySelector('.admin-nav nav').append(costNav);
const notificationNav = document.createElement('button'); notificationNav.dataset.view = 'notifications'; notificationNav.textContent = '通知交付'; document.querySelector('.admin-nav nav').append(notificationNav); titles.notifications = '通知交付';
function access(mode) { if(mode!=='admin'){bootstrapUI.clear();adminNodeMonitoringPoller?.stop();adminNodeMonitoringPoller=undefined;} $('#admin-login').hidden = mode !== 'login'; $('#admin-denied').hidden = mode !== 'denied'; $('#admin-shell').hidden = mode !== 'admin'; }
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
function upgradePresentation(node) {
  const upgrade = node.upgrade; const current = node.agentVersion || '未报告';
  const fixedTarget = node.upgradeTargetVersion || '未配置';
  if (!upgrade) return { label: `当前版本 ${current}`, detail: `目标版本 ${fixedTarget} · 尚无升级记录` };
  const target = upgrade.targetVersion || fixedTarget; const status = upgradeStates[upgrade.state] || '未知状态';
  const error = upgrade.errorCode ? (upgradeErrors[upgrade.errorCode] || '未知错误，需要查看服务日志') : '';
  const changed = upgrade.updatedAt ? new Date(upgrade.updatedAt).toLocaleString() : '时间未知';
  return { label: `当前版本 ${current}`, detail: `目标版本 ${target} · ${status} · ${changed}${error ? ` · ${error}` : ''}` };
}
function appendUpgradeControls(controls, node) {
  const active = ['queued', 'claimed', 'installing', 'verifying'].includes(node.upgrade?.state);
  if (!active && node.upgradeEligible) controls.append(button('升级到当前固定版本', () => openNodeAction('upgrade', node)));
  if (['queued', 'claimed'].includes(node.upgrade?.state)) controls.append(button('取消升级', () => openNodeAction('upgrade-cancel', node)));
  if (['installing', 'verifying'].includes(node.upgrade?.state)) { const locked = button('升级中，操作已锁定', async () => {}); locked.disabled = true; controls.append(locked); }
}
function showView(view) { state.view = view; for (const pane of document.querySelectorAll('[data-pane]')) pane.hidden = pane.dataset.pane !== view; for (const nav of document.querySelectorAll('[data-view]')) nav.classList.toggle('selected', nav.dataset.view === view); $('#view-title').textContent = titles[view]; if (view === 'deliveries') loadDeliveries().catch(error => message(error.message)); if(view==='plans')loadPlans().catch(e=>message(e.message));if(view==='operations')operations.refresh().catch(e=>message(e.message)); }
bindAdminNavigation({ root: document.querySelector('.admin-nav'), toggle: $('#admin-nav-toggle'), title: $('#view-title'), items: document.querySelectorAll('[data-view]'), documentTarget: document, onSelect: showView });
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
function renderSummary(data) { $('#stat-tenants').textContent = data.tenants; $('#stat-suspended').textContent = `${data.suspendedTenants} 家已停用`; $('#stat-nodes').textContent = data.nodes; $('#stat-maintenance').textContent = `${data.maintenanceNodes} 个维护中`; $('#stat-rules').textContent = data.rules; $('#stat-paused').textContent = `${data.pausedRules} 条已暂停`; $('#stat-failed').textContent = data.failedDeliveries; }
async function mutate(path, body, success) {
  try { await api(path, body); }
  catch (error) { if (error.status === 409) { await reload(); const nodeId = /^\/api\/admin\/nodes\/([^/]+)\//.exec(path)?.[1]; const latest = state.nodes.find(node => node.id === nodeId); const detail = path.endsWith('/start-draining') ? '节点必须先进入维护；排空一旦开始不可恢复分配。' : path.endsWith('/finalize-retirement') ? `尚未满足最终退役门禁：${retirementReasonText(latest?.retirementReasons).join('；') || '状态已经变化'}。` : path.endsWith('/withdraw-registration') ? '节点已进入部署或业务生命周期，不能撤销登记；请改用安全退役。' : '有规则的节点不可更换绑定地址或降低至规则速率以下。'; message(`操作冲突，已载入最新数据，请重新确认后操作。${detail}`); return false; } throw error; }
  message(success);
  try { await reload(); } catch (error) { message(`${success} 列表刷新失败，请点击刷新查看结果：${error.message}`); }
  return true;
}
const retirementReasons = { status: '节点尚未进入排空', rules: '仍有未删除规则', allocations: '仍有端口占用', retirements: '仍有端口清理任务', generation: '最新配置尚未应用确认', 'configuration-active': '配置交付仍在进行', 'upgrade-active': 'Agent 升级仍在进行', 'rotation-pending': '证书轮换仍在进行', attestation: '等待最新 Agent 排空证明' };
function retirementReasonText(reasons = []) { return reasons.map(reason => retirementReasons[reason] || `未知门禁 ${reason}`); }
function nodeResourcePresentation(node) {
  if (node.status === 'retired') return { label: '已退役 · 身份已撤销', detail: '历史与隔离证据保留；不可恢复' };
  if (node.status === 'draining') { const reasons = retirementReasonText(node.retirementReasons); return node.retirementEligible ? { label: '排空完成 · 可最终退役', detail: '最新本地证明已通过全部门禁' } : { label: '排空中', detail: reasons.join('；') || '等待控制面重新核验' }; }
  if (node.status === 'maintenance') return { label: '维护中', detail: '停止新增分配；已有流量不会自动停止' };
  if (node.status === 'ready') return { label: '可分配', detail: node.appliedGeneration > 0 ? '已有应用确认 · 端到端健康未知' : '尚无应用确认' };
  return { label: '尚未部署', detail: '仅登记了资源' };
}
function deliveryControl(node) { return button('交付记录', () => deliveryDialog.open(node)); }
function openNodeAction(kind, node, onSuccess) {
  const action = nodeActionDescriptor(kind, node);
  if (!action) return;
  nodeActionDialog.open({
    ...action,
    run: createNodeActionRunner({ kind, node, getNode: id => state.nodes.find(item => item.id === id), mutate, conflictMessage: () => $('#admin-message').textContent, onRefresh: refreshed => nodeActionDialog.update(refreshed), onSuccess }),
  });
}
function monitoringCell(row, nodeID, kind) {
  const target = row.insertCell(); target.setAttribute('data-monitoring', `${nodeID}:${kind}`); target.textContent = '等待数据'; return target;
}
function monitoringFact(target, primary, detail) {
  target.replaceChildren(); const value = document.createElement('strong'); value.textContent = primary; const note = document.createElement('span'); note.className = 'detail'; note.textContent = detail; target.append(value, note);
}
function monitoringMetric(target, label, metric) {
  const item = document.createElement('div'); item.className = `admin-node-metric ${metric.value === null ? 'is-unknown' : ''}`;
  const name = document.createElement('span'); name.textContent = label; const value = document.createElement('strong'); value.textContent = metric.label;
  const track = document.createElement('i'); const fill = document.createElement('b'); if (metric.value !== null) fill.style.width = `${metric.value}%`; track.append(fill); item.append(name, value, track); target.append(item);
}
function patchAdminNodeMonitoring(items, { error = '', refreshedAt = new Date() } = {}) {
  const messageTarget = $('#admin-node-monitoring-message');
  if (error) { messageTarget.className = 'error hint'; messageTarget.textContent = `节点监控刷新失败：${error}。保留上次成功数据。`; return false; }
  state.monitoring = new Map(items.map(node => [node.id, node]));
  for (const node of items) {
    const row = $(`#admin-nodes tr[data-node-id="${node.id}"]`); if (!row) continue;
    const view = nodeMonitoringPresentation(node, refreshedAt);
    monitoringFact(row.querySelector(`[data-monitoring="${node.id}:rate"]`), `↑ ${view.traffic.uploadRate}`, `↓ ${view.traffic.downloadRate}`);
    monitoringFact(row.querySelector(`[data-monitoring="${node.id}:traffic"]`), `↑ ${view.traffic.uploadTotal}`, `↓ ${view.traffic.downloadTotal}`);
    monitoringFact(row.querySelector(`[data-monitoring="${node.id}:runtime"]`), view.agent.runtime, 'Agent 本次进程');
    const resources = row.querySelector(`[data-monitoring="${node.id}:resources"]`); resources.replaceChildren(); resources.className = 'admin-node-resources';
    monitoringMetric(resources, 'CPU', view.resources.cpu); monitoringMetric(resources, 'RAM', view.resources.memory); monitoringMetric(resources, '存储', view.resources.disk); monitoringMetric(resources, '连接跟踪', view.resources.conntrack);
    monitoringFact(row.querySelector(`[data-monitoring="${node.id}:observed"]`), `流量 ${view.lastReportedAt}`, `资源 ${view.resources.observedAt}`);
  }
  messageTarget.className = 'muted hint'; messageTarget.textContent = `最近刷新：${refreshedAt.toLocaleTimeString()}。动态数据不代表端到端健康。`; return true;
}
function startAdminNodeMonitoring() {
  if (adminNodeMonitoringPoller) return;
  adminNodeMonitoringPoller = createAdaptivePoller({ run: async () => {
    try { const result = await api('/api/admin/operations/nodes?staleAfterSeconds=90'); patchAdminNodeMonitoring(result.items || [], { refreshedAt: new Date() }); }
    catch (error) { patchAdminNodeMonitoring([], { error: error.message }); throw error; }
  } });
  adminNodeMonitoringPoller.start();
}
function renderNodes(nodes) {
  const root = $('#admin-nodes'); root.replaceChildren();
  for (const node of nodes) {
    const row = root.insertRow(); row.dataset.nodeId = node.id; cell(row, node.name, node.id); cell(row, node.region, node.publicAddress || '计划公网 IP 未填写'); cell(row, `${node.capacityMbps} Mbps`);
    const resource = nodeResourcePresentation(node); cell(row, resource.label, resource.detail);
    const contact = agentContactPresentation(node.contactStatus); const agentDetail = `${contact.detail}${node.agentMode ? `；${node.agentMode}` : ''}`; const contactCell = cell(row, contact.label, agentDetail); contactCell.classList.add('agent-status', `agent-status-${contact.tone}`);
    const resourceStatus = resourcePresentation(node); const resourceCell = cell(row, resourceStatus.label, resourceStatus.detail); resourceCell.classList.add('agent-status', `agent-status-${resourceStatus.tone}`);
    monitoringCell(row, node.id, 'rate'); monitoringCell(row, node.id, 'traffic'); monitoringCell(row, node.id, 'runtime'); monitoringCell(row, node.id, 'resources'); monitoringCell(row, node.id, 'observed');
    const upgrade = upgradePresentation(node); const upgradeCell = cell(row, upgrade.label, upgrade.detail); upgradeCell.classList.add('upgrade-status'); const configuration = configurationPresentation(node); cell(row, configuration.label, configuration.detail); cell(row, node.deliveryState === 'failed' && node.deliveryAttempts >= 5 ? '重试耗尽 · 待处理' : deliveries[node.deliveryState] || '未知');
    const probe = probePresentation(node); const probeCell = cell(row, probe.label, probe.detail); probeCell.classList.add('agent-status', `agent-status-${probe.tone}`); const certificate = certificatePresentation(node); const certificateCell = cell(row, certificate.label, certificate.detail); certificateCell.classList.add('agent-status', `agent-status-${certificate.tone}`);
    const controls = row.insertCell(); controls.className = 'table-actions';
    if (node.status === 'retired') { controls.append(deliveryControl(node)); continue; }
    if (node.status === 'draining') {
      controls.append(button('重试交付', () => openNodeAction('retry', node)), deliveryControl(node));
      if (node.retirementEligible) controls.append(button('最终退役节点', () => openNodeAction('finalize-retirement', node), true));
      else { const waiting = button('等待安全退役条件', async () => {}); waiting.disabled = true; controls.append(waiting); }
      continue;
    }
    controls.append(button('编辑', () => editNode(node)), button(node.status === 'maintenance' ? '恢复分配' : '进入维护', () => openNodeAction(node.status === 'maintenance' ? 'enable' : 'maintenance', node)), button('证书', () => editCertificate(node)), button('重试交付', () => openNodeAction('retry', node)), deliveryControl(node));
    appendUpgradeControls(controls, node);
    if (node.status === 'maintenance') controls.append(button('进入安全排空', () => openNodeAction('start-draining', node), true));
    controls.append(button('安装 Agent',()=>bootstrapUI.open(node)));
    if (node.withdrawable) controls.append(button('撤销登记', () => openNodeAction('withdraw-registration', node), true));
  }
  empty(root, 17, '暂无节点，请先登记入口资源。');
  if (state.monitoring.size) patchAdminNodeMonitoring([...state.monitoring.values()]);
}
const nodeFormMessage = $('#node-form-message');
const nodeActionDialog = bindNodeActionDialog({ dialog: $('#node-action-dialog'), title: $('#node-action-title'), description: $('#node-action-description'), status: $('#node-action-status'), confirmButton: $('#node-action-confirm'), cancelButton: $('#node-action-cancel') });
const deliveryDialog = bindNodeDeliveryDialog({ dialog: $('#node-delivery-dialog'), closeButton: $('#node-delivery-close'), title: $('#node-delivery-title'), status: $('#node-delivery-status'), load: id => api(`/api/admin/nodes/${encodeURIComponent(id)}/deliveries`), render: records => renderDeliveryRows($('#node-delivery-rows'), records) });
const nodeEditor = bindNodeEditor({ dialog: $('#node-dialog'), form: $('#node-form'), title: $('#node-form-title'), save: $('#node-save'), status: nodeFormMessage, createButton: $('#node-new'), cancelButton: $('#node-dialog-cancel') });
const certificateDialog = bindNodeContentDialog({ dialog: $('#certificate-dialog'), closeButton: $('#certificate-close'), onClose: () => { state.certificateNode = null; $('#certificate-form').reset(); $('#certificate-message').textContent = ''; } });
function editNode(node) { nodeEditor.openEdit(node); }
$('#node-form').addEventListener('submit', async event => { event.preventDefault(); const submit = $('#node-save'); submit.disabled = true; nodeFormMessage.textContent = ''; const input = Object.fromEntries(new FormData(event.target)); input.capacityMbps = Number(input.capacityMbps); const editing = nodeEditor.current(); if (editing) input.revision = editing.revision; await runDialogSession(nodeEditor, () => mutate(editing ? `/api/admin/nodes/${editing.id}/update` : '/api/admin/nodes', input, editing ? '节点资料已更新。' : '节点已登记，尚未部署实际转发。'), { success: async saved => { if (saved) { nodeEditor.close(); if (!editing) await bootstrapUI.open(state.nodes.find(node => node.id === input.id)); } else { const latest = editing && state.nodes.find(node => node.id === editing.id); if (latest) editNode(latest); nodeFormMessage.textContent = $('#admin-message').textContent; } }, error: error => { message(error.message); nodeFormMessage.textContent = error.message; }, finally: () => { submit.disabled = false; } }); });
function editCertificate(node) { state.certificateNode = node; $('#certificate-node').textContent = node.name; $('#certificate-fingerprint').textContent = node.certificateFingerprint || '未绑定'; $('#certificate-revoke').disabled = !node.certificateFingerprint; $('#certificate-form button').disabled = false; $('#certificate-message').textContent = ''; $('#certificate-form').reset(); certificateDialog.open(node); }
$('#certificate-form').addEventListener('submit', async event => { event.preventDefault(); const node = state.certificateNode; if (!node) return; const submit = event.target.querySelector('button'); submit.disabled = true; $('#certificate-message').textContent = ''; await runDialogSession(certificateDialog, () => mutate(`/api/admin/nodes/${node.id}/enroll`, { revision: node.revision, certificate: event.target.elements.namedItem('certificate').value }, '证书已绑定，旧授权已失效，最新配置已重新入队。'), { success: saved => { if (saved) certificateDialog.close(); else { const latest = state.nodes.find(item => item.id === node.id); if (latest) editCertificate(latest); $('#certificate-message').textContent = $('#admin-message').textContent; } }, error: error => { message(error.message); $('#certificate-message').textContent = error.message; }, finally: () => { submit.disabled = false; } }); });
$('#certificate-revoke').addEventListener('click', () => { const node = state.certificateNode; if (node) openNodeAction('revoke-certificate', node, () => certificateDialog.close()); });

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
function renderDeliveryRows(root, records) { root.replaceChildren(); for (const record of records) { const row = root.insertRow(); for (const value of [record.generation, deliveries[record.state] || record.state, record.attempts, ({ 'invalid-config': '配置检查失败', 'internal-error': 'Agent 内部错误', 'lease-exhausted': '租约重试耗尽' })[record.errorCode] || record.errorCode || '—', new Date(record.createdAt).toLocaleString(), record.reportedAt ? new Date(record.reportedAt).toLocaleString() : '尚未回报']) cell(row, value); } empty(root, 6, '暂无交付记录。'); }
async function loadDeliveries() { const root = $('#admin-deliveries'); const id = $('#delivery-node').value; if (!id) { root.replaceChildren(); empty(root, 6, '请先登记节点。'); return; } renderDeliveryRows(root, await api(`/api/admin/nodes/${encodeURIComponent(id)}/deliveries`)); }
$('#delivery-node').addEventListener('change', () => loadDeliveries().catch(error => message(error.message)));
$('#rule-filters').addEventListener('submit', async event => { event.preventDefault(); state.nodeFilter = $('#rules-node').value; state.rulePage = 1; try { await reload(); } catch (error) { message(error.message); } });
$('#rules-clear-tenant').addEventListener('click', async () => { state.tenantFilter = ''; state.tenantName = ''; state.rulePage = 1; try { await reload(); } catch (error) { message(error.message); } });
for (const kind of ['tenants', 'rules']) for (const direction of ['prev', 'next']) $(`#${kind}-${direction}`).addEventListener('click', async () => { const key = kind === 'tenants' ? 'tenantPage' : 'rulePage'; state[key] += direction === 'next' ? 1 : -1; try { await reload(); } catch (error) { message(error.message); } });
async function enter() { const who = await api('/api/auth/me'); if (!who.platformAdmin) { access('denied'); return; } $('#admin-email').textContent = who.email; access('admin'); await reload(); startAdminNodeMonitoring(); }
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
const bootstrapUI = bindNodeBootstrap({api,getNode:id=>state.nodes.find(node=>node.id===id),reload,message});
enter().catch(error => { if (error.status !== 401 && error.status !== 403) { access('login'); $('#login-message').textContent = error.message; } });
