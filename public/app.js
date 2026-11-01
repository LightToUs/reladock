import { submitRule, submitRuleUpdate } from './rule-submission.js';
import { createRuleEditor } from './rule-editor.js';
import { workspaceSummary, ruleStatusPresentation } from './workspace-summary.js';
import { createLatestRequestGate, loadWorkspaceData, telemetryInteractionState, telemetryPresentation, trendBars, formatBytes } from './telemetry.js';
import { resourcePresentation } from './node-contact-status.js';
import { createAdaptivePoller, nodeMonitoringPresentation } from './node-monitoring.js';
import { bindAccountTools,refreshAccountTools,clearCredential } from './account-tools.js';
import { bindBusinessTools,refreshBusinessTools } from './business-tools.js';
import { bindOperations } from './operations-tools.js';
const $ = selector => document.querySelector(selector);
let mode = 'login';
let identity;
const ruleDraft = {};
let telemetryByRule = new Map();
let telemetrySummariesAvailable = false;
let nodeMonitoringPoller;
const telemetryHistoryGate = createLatestRequestGate();
const ruleEditor = createRuleEditor($('#rule-dialog'), $('#rule-form'), {
  title: $('#rule-dialog-title'), description: $('#rule-dialog-description'), submit: $('#rule-submit'),
  status: $('#rule-dialog-status'), allocation: $('#rule-allocation'),
  createButton: $('#rule-create'), cancelButton: $('#rule-dialog-cancel'),
});
$('#rule-dialog-close').addEventListener('click', ruleEditor.close);

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
function showAuth() { clearCredential();identity = undefined; nodeMonitoringPoller?.stop(); nodeMonitoringPoller = undefined; ruleDraft.pending = undefined; ruleEditor.close(); $('#startup-error').hidden = true; $('#auth').hidden = false; $('#console').hidden = true; for (const id of ['rules-message','refresh-message','account-tools-message','rule-dialog-status']) $(`#${id}`).textContent = ''; }
function showStartupFailure(error) { $('#auth').hidden = true; $('#console').hidden = true; $('#startup-error').hidden = false; $('#startup-error-message').textContent = error.message; }
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
const actions = { 'account.register': '开通企业', 'account.login': '登录', 'account.logout': '退出登录', 'rule.create': '创建规则', 'rule.update': '编辑规则', 'rule.pause': '暂停配置', 'rule.resume': '恢复配置', 'rule.delete': '删除配置' };
const deliveryStates = { queued: '等待领取', leased: '领取处理中', applying: '应用中 · 等待确认', applied: '已应用 · 健康未验证', validated: '校验通过 · 未应用', failed: '配置失败 / 待重试', superseded: '已被新版本替代' };

function appendTelemetryDetail(target, primary, detail) {
  target.textContent = primary;
  const note = document.createElement('small'); note.className = 'telemetry-detail'; note.textContent = detail; target.append(note);
}

function appendNodeMetric(target, label, metric) {
  const item = document.createElement('div'); item.className = `node-metric ${metric.value === null ? 'is-unknown' : ''}`;
  const heading = document.createElement('span'); heading.textContent = label;
  const value = document.createElement('strong'); value.textContent = metric.label;
  const track = document.createElement('i'); const fill = document.createElement('b');
  if (metric.value !== null) fill.style.width = `${metric.value}%`;
  track.append(fill); item.append(heading, value, track); target.append(item);
}

function appendNodeFact(target, primary, detail) {
  const value = document.createElement('strong'); value.textContent = primary;
  const note = document.createElement('small'); note.textContent = detail;
  target.append(value, note);
}

function renderEnterpriseNodeMonitoring(items, { error = '', refreshedAt = new Date() } = {}) {
  const message = $('#node-status-message');
  if (error) {
    message.className = 'error hint';
    message.textContent = `节点状态刷新失败：${error}。保留上次成功数据。`;
    return false;
  }
  const root = $('#node-status-rows'); root.replaceChildren();
  if (!items.length) {
    const empty = root.insertRow().insertCell(); empty.colSpan = 7; empty.className = 'table-empty'; empty.textContent = '当前企业尚无关联入口节点。';
  }
  for (const node of items) {
    const view = nodeMonitoringPresentation(node, refreshedAt);
    const row = root.insertRow(); row.dataset.nodeId = node.id;
    const identityCell = row.insertCell(); appendNodeFact(identityCell, node.name, node.id);
    const stateCell = row.insertCell();
    const badge = document.createElement('span'); badge.className = `badge badge-${view.contact.tone}`; badge.textContent = view.contact.label;
    const plane = document.createElement('small'); plane.textContent = `配置：${view.dataPlane.state}`; plane.title = view.dataPlane.detail; stateCell.append(badge, plane);
    const rateCell = row.insertCell(); appendNodeFact(rateCell, `↑ ${view.traffic.uploadRate}`, `↓ ${view.traffic.downloadRate}`); rateCell.title = view.traffic.detail;
    const trafficCell = row.insertCell(); appendNodeFact(trafficCell, `↑ ${view.traffic.uploadTotal}`, `↓ ${view.traffic.downloadTotal}`);
    const runtimeCell = row.insertCell(); appendNodeFact(runtimeCell, view.agent.runtime, 'Agent 本次进程'); runtimeCell.title = view.agent.detail;
    const resourceCell = row.insertCell(); resourceCell.className = 'node-resource-metrics';
    appendNodeMetric(resourceCell, 'CPU', view.resources.cpu); appendNodeMetric(resourceCell, 'RAM', view.resources.memory); appendNodeMetric(resourceCell, '存储', view.resources.disk); appendNodeMetric(resourceCell, '连接跟踪', view.resources.conntrack);
    const observedCell = row.insertCell(); appendNodeFact(observedCell, `流量 ${view.lastReportedAt}`, `资源 ${view.resources.observedAt}`);
  }
  message.className = 'muted hint';
  message.textContent = `最近刷新：${refreshedAt.toLocaleTimeString()}。失败时保留上次成功值。`;
  return true;
}

function startNodeMonitoring() {
  if (nodeMonitoringPoller) return;
  nodeMonitoringPoller = createAdaptivePoller({
    run: async () => {
      try {
        const result = await api('/api/operations/nodes?staleAfterSeconds=90');
        renderEnterpriseNodeMonitoring(result.items || [], { refreshedAt: new Date() });
      } catch (error) {
        renderEnterpriseNodeMonitoring([], { error: error.message });
        throw error;
      }
    },
  });
  nodeMonitoringPoller.start();
}

function renderNodeResources(nodes, available = true) {
  const root = $('#node-resource-cards'); root.replaceChildren();
  if (!available) {
    const unavailable = document.createElement('p'); unavailable.className = 'muted telemetry-empty'; unavailable.textContent = '节点资源遥测暂不可用，关联状态与资源占用均为未知。'; root.append(unavailable); return;
  }
  if (!nodes.length) {
    const empty = document.createElement('p'); empty.className = 'muted telemetry-empty'; empty.textContent = '当前规则尚未关联可展示的入口节点资源。'; root.append(empty); return;
  }
  for (const node of nodes) {
    const view = resourcePresentation(node);
    const card = document.createElement('section'); card.className = `resource-card resource-${view.tone}`;
    const heading = document.createElement('div'); heading.className = 'resource-heading';
    const name = document.createElement('strong'); name.textContent = node.name;
    const badge = document.createElement('span'); badge.className = `badge badge-${view.tone}`; badge.textContent = view.label;
    heading.append(name, badge);
    const metrics = document.createElement('dl');
    for (const [label, value] of [['CPU', node.cpuPercent], ['内存', node.memoryPercent], ['磁盘', node.diskPercent], ['连接跟踪', node.conntrackPercent]]) {
      const group = document.createElement('div'); const term = document.createElement('dt'); const fact = document.createElement('dd');
      term.textContent = label; fact.textContent = Number.isInteger(value) ? `${value}%` : '未知'; group.append(term, fact); metrics.append(group);
    }
    const detail = document.createElement('p'); detail.className = 'hint muted'; detail.textContent = view.detail;
    card.append(heading, metrics, detail); root.append(card);
  }
}

function setTelemetrySummary(summary) {
  const view = telemetryPresentation(summary);
  $('#telemetry-total').textContent = view.total;
  $('#telemetry-total-detail').textContent = summary?.hasReports ? `上行 ${view.upload} / 下行 ${view.download}` : '等待流量报告';
  $('#telemetry-upload-rate').textContent = view.uploadRate;
  $('#telemetry-download-rate').textContent = view.downloadRate;
  $('#telemetry-utilization').textContent = view.utilization;
  const state = $('#telemetry-rate-state'); state.className = `telemetry-state telemetry-state-${view.rateTone}`; state.textContent = view.rate;
  state.title = view.rateDetail;
}

function setTelemetryUnavailable() {
  $('#telemetry-total').textContent = '遥测不可用'; $('#telemetry-total-detail').textContent = '本次加载失败，累计流量未知';
  $('#telemetry-upload-rate').textContent = '未知'; $('#telemetry-download-rate').textContent = '未知'; $('#telemetry-utilization').textContent = '未知';
  const state = $('#telemetry-rate-state'); state.className = 'telemetry-state telemetry-state-stale'; state.textContent = '遥测不可用'; state.title = '本次加载失败，不表示没有流量。';
}

function renderTrafficTrend(series) {
  const root = $('#traffic-trend'); root.replaceChildren();
  const trend = trendBars(series.points, series.bucketSeconds);
  root.style.gridTemplateColumns = `repeat(${Math.max(1, trend.slotCount)}, minmax(7px, 1fr))`;
  $('#traffic-trend-message').textContent = trend.bars.length
    ? `${trend.bars.length} 个已接收数据桶 · 每桶 ${series.bucketSeconds.toLocaleString()} 秒`
    : '该窗口内没有已接收的流量报告；不推断为 0。';
  if (!trend.bars.length) {
    const empty = document.createElement('div'); empty.className = 'trend-empty'; empty.textContent = '暂无已接收数据'; root.append(empty); return;
  }
  for (const point of trend.bars) {
    const bar = document.createElement('div'); bar.className = 'trend-bar';
    bar.style.gridColumn = String(point.slot + 1);
    const fill = document.createElement('i'); fill.style.height = `${Math.max(3, point.ratio * 100)}%`;
    const timestamp = new Date(point.startedAt).toLocaleString();
    bar.title = `${timestamp} · 总计 ${formatBytes(point.totalBytes)}（上行 ${formatBytes(point.uploadBytes)} / 下行 ${formatBytes(point.downloadBytes)}）`;
    bar.setAttribute('aria-label', bar.title); bar.append(fill); root.append(bar);
  }
}

function renderTrafficUnavailable() {
  const root = $('#traffic-trend'); root.replaceChildren(); root.style.gridTemplateColumns = '1fr';
  const unavailable = document.createElement('div'); unavailable.className = 'trend-empty'; unavailable.textContent = '流量遥测暂不可用'; root.append(unavailable);
  $('#traffic-trend-message').textContent = '本次加载失败，无法判断该窗口是否有报告；不推断为 0。';
}

async function refreshTelemetryHistory() {
  const ruleID = $('#telemetry-rule').value; const window = $('#telemetry-window').value;
  const requestToken = telemetryHistoryGate.next();
  const interaction = telemetryInteractionState(telemetrySummariesAvailable, ruleID);
  if (!interaction.historyAllowed) {
    if (!telemetrySummariesAvailable) { setTelemetryUnavailable(); renderTrafficUnavailable(); return; }
    setTelemetrySummary(); renderTrafficTrend({ points: [], bucketSeconds: 0 }); return;
  }
  setTelemetrySummary(telemetryByRule.get(ruleID));
  $('#telemetry-message').textContent = '';
  try {
    const series = await api(`/api/billing/telemetry?ruleId=${encodeURIComponent(ruleID)}&window=${encodeURIComponent(window)}`);
    if (!telemetryHistoryGate.isCurrent(requestToken) || $('#telemetry-rule').value !== ruleID || $('#telemetry-window').value !== window) return;
    setTelemetrySummary(series.summary); renderTrafficTrend(series);
  } catch (error) {
    if (!telemetryHistoryGate.isCurrent(requestToken)) return;
    $('#telemetry-message').textContent = error.message;
    $('#traffic-trend-message').textContent = '趋势数据加载失败，累计值仍保留。';
  }
}

function renderTelemetryWorkspace(rules, telemetry, operationNodes, { telemetryAvailable, resourcesAvailable }) {
  telemetrySummariesAvailable = telemetryAvailable;
  telemetryByRule = new Map(telemetry.map(item => [item.ruleId, item]));
  const selector = $('#telemetry-rule'); const selected = selector.value; selector.replaceChildren();
  for (const rule of rules) {
    const option = document.createElement('option'); option.value = rule.id; option.textContent = `${rule.name} · ${rule.protocol.toUpperCase()} ${rule.listenPort}`; selector.append(option);
  }
  if (rules.some(rule => rule.id === selected)) selector.value = selected;
  const interaction = telemetryInteractionState(telemetryAvailable, selector.value);
  selector.disabled = interaction.controlsDisabled;
  $('#telemetry-window').disabled = interaction.controlsDisabled;
  if (telemetryAvailable) setTelemetrySummary(telemetryByRule.get(selector.value));
  else setTelemetryUnavailable();
  renderNodeResources(operationNodes, resourcesAvailable);
}

async function refresh() {
  const workspace = await loadWorkspaceData(api);
  const data = workspace.overview; const telemetry = workspace.telemetry;
  identity = data.account;
  $('#startup-error').hidden = true; $('#auth').hidden = true; $('#console').hidden = false; $('#company-name').textContent = identity.company; $('#account-email').textContent = identity.email;
  $('#platform-link').hidden = !identity.platformAdmin;
  const summary = workspaceSummary(data);
  $('#summary-rule-total').textContent = summary.totalRules.toLocaleString();
  $('#summary-rule-applied').textContent = summary.appliedRules.toLocaleString();
  $('#summary-rule-pending').textContent = summary.pendingRules.toLocaleString();
  $('#summary-node-contact').textContent = `${summary.contactedNodes}/${summary.totalNodes}`;
  const editable = ['owner', 'admin'].includes(identity.role); $('#rule-create').disabled = !editable;
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
  $('#rule-create').disabled = !editable || !selector.value;
  if ($('#rule-dialog').open) $('#rule-submit').disabled = !editable || (!ruleEditor.current() && !selector.value);
  $('#refresh-message').textContent = `最近刷新：${new Date().toLocaleTimeString()}。应用确认不等于端到端健康。`;
  const root = $('#rules'); root.replaceChildren(); $('#rule-count').textContent = `(${data.rules.length})`;
  if (!data.rules.length) { const c = root.insertRow().insertCell(); c.colSpan = 10; c.className = 'table-empty'; c.textContent = '暂无规则，请填写客户的目标服务器。'; }
  for (const rule of data.rules) {
    const row = root.insertRow(); const target = rule.targetHost.includes(':') ? `[${rule.targetHost}]:${rule.targetPort}` : `${rule.targetHost}:${rule.targetPort}`;
    for (const value of [`${rule.name} / ${rule.nodeId}`, rule.listenPort, rule.protocol.toUpperCase(), target,`${rule.bandwidthMbps} Mbps`]) cell(row, value);
    const telemetryView = workspace.telemetryAvailable
      ? telemetryPresentation(telemetry.find(item => item.ruleId === rule.id))
      : { total: '遥测不可用', upload: '未知', download: '未知', rate: '未知', rateDetail: '本次加载失败，不表示没有流量。' };
    const usageDetail = !workspace.telemetryAvailable ? telemetryView.rateDetail : telemetryView.total === '尚未收到流量报告' ? '等待节点计量上报' : `上行 ${telemetryView.upload} / 下行 ${telemetryView.download}`;
    const usageCell = row.insertCell(); appendTelemetryDetail(usageCell, telemetryView.total, usageDetail);
    const rateCell = row.insertCell(); appendTelemetryDetail(rateCell, telemetryView.rate, telemetryView.rateDetail);
    cell(row,`期望 ${rule.desiredVersion} / 生效 ${rule.appliedVersion}`);
    const presentation = ruleStatusPresentation(rule);
    const statusCell = row.insertCell();
    const badge = document.createElement('span'); badge.className = `badge badge-${presentation.tone}`; badge.textContent = presentation.label; statusCell.append(badge);
    const detail = document.createElement('small'); detail.className = 'status-detail'; detail.textContent = presentation.detail; statusCell.append(detail);
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
    statusCell.append(probeButton);
    const controls = row.insertCell(); controls.className = 'table-actions';
    if (editable) {
      const editButton = document.createElement('button'); editButton.className = 'secondary'; editButton.textContent = '编辑'; editButton.addEventListener('click', () => ruleEditor.openEdit(rule)); controls.append(editButton);
      controls.append(actionButton(rule.status === 'paused' ? '恢复' : '暂停', rule.id, rule.status === 'paused' ? 'resume' : 'pause'));
      controls.append(actionButton('删除', rule.id, 'delete'));
    }
  }
  renderTelemetryWorkspace(data.rules, telemetry, workspace.operationNodes, workspace);
  renderEnterpriseNodeMonitoring(workspace.operationNodes, { refreshedAt: new Date() });
  startNodeMonitoring();
  const telemetryUnavailable = workspace.errors.some(item => item.path === '/api/billing/telemetry');
  $('#telemetry-message').textContent = workspace.errors.map(item => `${item.path.includes('nodes') ? '节点资源' : '流量遥测'}暂不可用：${item.error.message}`).join('；');
  if (telemetryUnavailable) renderTrafficUnavailable();
  else await refreshTelemetryHistory();
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
  event.preventDefault();
  const button = $('#rule-submit'); const editing = ruleEditor.current(); const session = ruleEditor.session(); button.disabled = true;
  const fields = Object.fromEntries(new FormData(event.target)); fields.targetPort = Number(fields.targetPort); fields.bandwidthMbps = Number(fields.bandwidthMbps);
  delete fields.listenPort;
  try {
    let result;
    if (editing) {
      const update = { name: fields.name, targetHost: fields.targetHost, targetPort: fields.targetPort, bandwidthMbps: fields.bandwidthMbps, desiredVersion: Number(fields.desiredVersion) };
      result = await submitRuleUpdate(update, body => api(`/api/rules/${editing.id}`, { method: 'PATCH', body }), refresh);
    } else {
      delete fields.desiredVersion;
      result = await submitRule(ruleDraft, fields, (body, key) => api('/api/rules', { method: 'POST', body, key }), refresh);
    }
    if (!ruleEditor.isCurrent(session)) return;
    const message = editing
      ? `规则“${result.rule.name}”已保存为期望版本 ${result.rule.desiredVersion}，等待 Agent 确认。`
      : `已预留 ${result.rule.protocol.toUpperCase()} 端口 ${result.rule.listenPort}。配置等待 Agent 交付，尚未转发流量。`;
    if (result.refreshError) $('#rule-dialog-status').textContent = `${message} 列表刷新失败，请关闭弹窗后重试刷新。`;
    else { $('#rules-message').textContent = message; ruleEditor.close(); }
  }
  catch (error) {
    if (ruleEditor.isCurrent(session)) $('#rule-dialog-status').textContent = error.status === 409 ? '规则已被其他操作更新。请关闭弹窗、刷新列表后重新编辑。' : error.message;
  } finally {
    if (ruleEditor.isCurrent(session)) button.disabled = !identity || !['owner', 'admin'].includes(identity.role) || (!editing && !$('#node-select').value);
  }
});
setMode('login');
bindAccountTools(api,refresh);
bindBusinessTools(api,refresh);
bindOperations(api,{canWrite:()=>identity&&['owner','admin'].includes(identity.role)});
$('#telemetry-rule').addEventListener('change', () => refreshTelemetryHistory());
$('#telemetry-window').addEventListener('change', () => refreshTelemetryHistory());
$('#startup-retry').addEventListener('click', () => location.reload());
$('#refresh').addEventListener('click', async () => { $('#refresh').disabled = true; try { await refresh(); } catch (error) { $('#refresh-message').textContent = error.message; } finally { $('#refresh').disabled = false; } });
refresh().catch(error => {
  if (error.status === 401) showAuth();
  else if (!identity) showStartupFailure(error);
  else $('#refresh-message').textContent = error.message;
});
