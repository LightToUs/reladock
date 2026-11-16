import { createAdaptivePoller } from './node-monitoring.js';

export const LIGHTSAIL_SCRIPT_WARNING = '脚本可留空；留空时只创建实例并绑定静态 IP，不开放 SSH。填写脚本时只执行一次；SSH 开始后若结果为 unknown（未知），平台不会自动重放。';

const lightsailRegionLocations = Object.freeze({
  'ap-east-1': '香港',
  'ap-northeast-1': '日本东京',
  'ap-northeast-2': '韩国首尔',
  'ap-south-1': '印度孟买',
  'ap-southeast-1': '新加坡',
  'ap-southeast-2': '澳大利亚悉尼',
  'ap-southeast-3': '印度尼西亚雅加达',
  'ap-southeast-5': '马来西亚',
  'ca-central-1': '加拿大中部',
  'eu-central-1': '德国法兰克福',
  'eu-north-1': '瑞典斯德哥尔摩',
  'eu-south-2': '西班牙',
  'eu-west-1': '爱尔兰',
  'eu-west-2': '英国伦敦',
  'eu-west-3': '法国巴黎',
  'sa-east-1': '巴西圣保罗',
  'us-east-1': '美国弗吉尼亚北部',
  'us-east-2': '美国俄亥俄',
  'us-west-2': '美国俄勒冈',
});

export function lightsailRegionLabel(region) {
  const code = String(region || '');
  const location = lightsailRegionLocations[code];
  return location ? `${location}（${code}）` : code;
}

const safeInstanceFields = [
  'id', 'name', 'selection', 'awsInstanceName', 'awsArn', 'staticIpName', 'staticIpAddress',
  'awsState', 'lifecycleState', 'deploymentState', 'scriptSha256', 'scriptSize',
  'executionStartedAt', 'hostKeySha256', 'executionOutput', 'exitCode', 'errorCode',
  'revision', 'createdAt', 'updatedAt', 'destroyedAt',
];

export function createLightsailPayload(fields = {}) {
  return {
    name: String(fields.name || '').trim(),
    selection: {
      region: String(fields.region || ''),
      availabilityZone: String(fields.availabilityZone || ''),
      blueprintId: String(fields.blueprintId || ''),
      bundleId: String(fields.bundleId || ''),
    },
    script: String(fields.script || ''),
  };
}

export function createLightsailSubmission({ post, keyFactory = () => globalThis.crypto.randomUUID() }) {
  let pending;
  return async fields => {
    const payload = createLightsailPayload(fields);
    const serialized = JSON.stringify(payload);
    if (!pending || pending.serialized !== serialized) pending = { serialized, key: keyFactory() };
    const result = await post(payload, pending.key);
    pending = undefined;
    return result;
  };
}

export function createCatalogLoader({ fetchCatalog, apply, onError = () => {} }) {
  let generation = 0;
  return {
    async load(region) {
      const request = ++generation;
      try {
        const catalog = await fetchCatalog(region);
        if (request === generation) apply(catalog);
        return catalog;
      } catch (error) {
        if (request === generation) onError(error);
        throw error;
      }
    },
    cancel() { generation += 1; },
  };
}

export function createActivationGate() {
  let generation = 0;
  let active = false;
  return {
    begin() {
      active = true;
      const request = ++generation;
      return { isCurrent: () => active && request === generation };
    },
    stop() { active = false; generation += 1; },
  };
}

export function statusPresentation(status = {}) {
  if (!status.enabled) return { label: '未启用', detail: '未配置 AWS Lightsail 连接器。' };
  return { label: '已启用', detail: '只允许平台配置中的 AWS 区域；浏览器不会接收 AWS 凭据。' };
}

export function instancePresentation(instance = {}) {
  const lifecycle = {
    creating: '正在创建实例', running: '实例运行中', stopping: '正在关机', stopped: '实例已关机',
    destroying: '正在销毁', destroyed: '实例已销毁', failed: '实例操作失败', 'manual-review': '需要人工复核',
  }[instance.lifecycleState] || '实例状态未知';
  const deployment = {
    pending: '等待执行脚本', running: '正在执行脚本', succeeded: '脚本执行成功', failed: '脚本执行失败',
    unknown: '脚本结果未知，不会重放', 'not-started': '脚本尚未开始', 'not-requested': '未要求执行脚本',
  }[instance.deploymentState] || '脚本状态未知';
  const actions = [];
  if (instance.lifecycleState === 'running') actions.push('stop');
  if (instance.lifecycleState === 'stopped') actions.push('destroy');
  return { lifecycle, deployment, detail: '不代表 RelaDock 转发或端到端健康', actions };
}

export function actionRequest(instance, action, confirmation = '') {
  if (!instance?.id || !Number.isInteger(instance.revision)) throw new Error('实例版本无效，请刷新后重试');
  if (action === 'stop') return { path: `/api/admin/lightsail/instances/${instance.id}/stop`, body: { revision: instance.revision } };
  if (action === 'destroy') {
    if (confirmation !== instance.awsInstanceName) throw new Error('必须完整输入实例名称后才能销毁');
    return { path: `/api/admin/lightsail/instances/${instance.id}/destroy`, body: { revision: instance.revision, confirmation } };
  }
  throw new Error('不支持的实例操作');
}

export function safeInstanceView(instance = {}) {
  const result = {};
  for (const key of safeInstanceFields) if (Object.hasOwn(instance, key)) result[key] = instance[key];
  return result;
}

export function setSafeText(node, value) {
  node.textContent = value == null ? '' : String(value);
}

export function pollingDelay(hidden) { return hidden ? 30000 : 5000; }

function option(documentTarget, value, label) {
  const result = documentTarget.createElement('option');
  result.value = value;
  result.textContent = label;
  return result;
}

function formatTime(value) {
  if (!value) return '—';
  const parsed = new Date(value);
  return Number.isFinite(parsed.valueOf()) ? parsed.toLocaleString('zh-CN', { hour12: false }) : '—';
}

export function bindLightsailAdmin({ api, documentTarget = globalThis.document, onMessage = () => {} }) {
  const find = selector => documentTarget.querySelector(selector);
  const statusRoot = find('#lightsail-status');
  const statusDetail = find('#lightsail-status-detail');
  const message = find('#lightsail-message');
  const createButton = find('#lightsail-create');
  const rows = find('#lightsail-instances');
  const createDialog = find('#lightsail-create-dialog');
  const createForm = find('#lightsail-create-form');
  const createMessage = find('#lightsail-create-message');
  const stopDialog = find('#lightsail-stop-dialog');
  const stopName = find('#lightsail-stop-name');
  const stopMessage = find('#lightsail-stop-message');
  const destroyDialog = find('#lightsail-destroy-dialog');
  const destroyName = find('#lightsail-destroy-name');
  const destroyInput = find('#lightsail-destroy-confirmation');
  const destroyMessage = find('#lightsail-destroy-message');
  const region = createForm?.elements.namedItem('region');
  const zone = createForm?.elements.namedItem('availabilityZone');
  const blueprint = createForm?.elements.namedItem('blueprintId');
  const bundle = createForm?.elements.namedItem('bundleId');
  let instances = new Map();
  let selected;
  let active = false;
  const activationGate = createActivationGate();

  const showMessage = value => { if (message) setSafeText(message, value); };
  const setSelect = (select, entries, label) => {
    if (!select) return;
    const saved = select.value;
    select.replaceChildren(...entries.map(entry => option(documentTarget, entry.value, entry.label)));
    if ([...select.options].some(entry => entry.value === saved)) select.value = saved;
    if (!select.options.length) select.append(option(documentTarget, '', label));
  };
  const applyCatalog = catalog => {
    setSelect(zone, (catalog.availabilityZones || []).filter(item => !item.state || item.state === 'available').map(item => ({ value: item.name, label: item.name })), '无可用区');
    setSelect(blueprint, (catalog.blueprints || []).map(item => ({ value: item.id, label: item.name ? `${item.name} · ${item.id}` : item.id })), '无 Linux 镜像');
    setSelect(bundle, (catalog.bundles || []).map(item => ({ value: item.id, label: `${item.name || item.id} · $${Number(item.priceUsd || 0).toFixed(2)}/月 · ${item.cpuCount || '?'} vCPU · ${item.ramGib || '?'} GiB` })), '无可用套餐');
    createButton.disabled = false;
  };
  const catalogLoader = createCatalogLoader({
    fetchCatalog: selectedRegion => api(`/api/admin/lightsail/catalog?region=${encodeURIComponent(selectedRegion)}`),
    apply: applyCatalog,
    onError: error => { createButton.disabled = true; showMessage(`目录加载失败：${error.message}`); },
  });

  const render = items => {
    instances = new Map(items.map(item => [item.id, safeInstanceView(item)]));
    rows?.replaceChildren();
    for (const item of instances.values()) {
      const presentation = instancePresentation(item);
      const row = rows.insertRow();
      const identity = row.insertCell();
      setSafeText(identity, item.name);
      const identityDetail = documentTarget.createElement('span');
      identityDetail.className = 'detail';
      setSafeText(identityDetail, `${item.awsInstanceName} · ${item.selection?.region || '区域未知'}`);
      identity.append(identityDetail);
      const address = row.insertCell();
      setSafeText(address, item.staticIpAddress || '静态 IP 待分配');
      const addressDetail = documentTarget.createElement('span');
      addressDetail.className = 'detail';
      setSafeText(addressDetail, item.staticIpName || 'IP 名称待创建');
      address.append(addressDetail);
      const lifecycle = row.insertCell();
      setSafeText(lifecycle, presentation.lifecycle);
      const lifecycleDetail = documentTarget.createElement('span');
      lifecycleDetail.className = 'detail';
      setSafeText(lifecycleDetail, presentation.detail);
      lifecycle.append(lifecycleDetail);
      const deployment = row.insertCell();
      setSafeText(deployment, presentation.deployment);
      const deploymentDetail = documentTarget.createElement('span');
      deploymentDetail.className = 'detail';
      setSafeText(deploymentDetail, item.errorCode || (item.executionOutput ? item.executionOutput.slice(0, 500) : (item.deploymentState === 'not-requested' ? '未配置脚本，未开放 SSH' : `脚本 ${item.scriptSize || 0} 字节 · ${formatTime(item.executionStartedAt)}`)));
      deployment.append(deploymentDetail);
      const controls = row.insertCell();
      controls.className = 'table-actions';
      for (const action of presentation.actions) {
        const control = documentTarget.createElement('button');
        control.type = 'button';
        control.className = `secondary${action === 'destroy' ? ' danger' : ''}`;
        control.textContent = action === 'stop' ? '关闭实例' : '销毁实例';
        control.addEventListener('click', () => {
          selected = item;
          if (action === 'stop') { setSafeText(stopName, item.awsInstanceName); setSafeText(stopMessage, ''); stopDialog.showModal(); }
          else { setSafeText(destroyName, item.awsInstanceName); destroyInput.value = ''; setSafeText(destroyMessage, ''); destroyDialog.showModal(); }
        });
        controls.append(control);
      }
    }
    if (rows && !rows.children.length) {
      const cell = rows.insertRow().insertCell();
      cell.colSpan = 5;
      cell.className = 'table-empty';
      cell.textContent = '暂无 Lightsail 实例。';
    }
  };

  const refreshInstances = async ({ signal, isCurrent = () => true } = {}) => {
    const result = await api('/api/admin/lightsail/instances', undefined, undefined, signal);
    if (isCurrent()) render(Array.isArray(result) ? result : []);
  };
  const poller = createAdaptivePoller({
    documentTarget,
    activeMs: pollingDelay(false),
    hiddenMs: pollingDelay(true),
    run: cycle => refreshInstances(cycle).catch(error => { if (cycle.isCurrent()) showMessage(`实例列表刷新失败，保留上次结果：${error.message}`); }),
  });
  const loadStatus = async isCurrent => {
    const status = await api('/api/admin/lightsail/status');
    if (!isCurrent()) return false;
    const presentation = statusPresentation(status);
    setSafeText(statusRoot, presentation.label);
    setSafeText(statusDetail, presentation.detail);
    createButton.disabled = !status.enabled;
    if (!status.enabled) { catalogLoader.cancel(); return false; }
    setSelect(region, (status.allowedRegions || []).map(value => ({ value, label: lightsailRegionLabel(value) })), '无允许区域');
    if (region.value) await catalogLoader.load(region.value);
    return isCurrent();
  };

  const submitCreate = createLightsailSubmission({ post: (payload, key) => api('/api/admin/lightsail/instances', payload, key) });
  region?.addEventListener('change', () => catalogLoader.load(region.value).catch(() => undefined));
  createButton?.addEventListener('click', () => { createForm.reset(); setSafeText(createMessage, LIGHTSAIL_SCRIPT_WARNING); if (region.value) catalogLoader.load(region.value).catch(() => undefined); createDialog.showModal(); });
  find('#lightsail-create-cancel')?.addEventListener('click', () => createDialog.close());
  createForm?.addEventListener('submit', async event => {
    event.preventDefault();
    const submit = createForm.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      await submitCreate(Object.fromEntries(new FormData(createForm)));
      createDialog.close();
      onMessage('Lightsail 创建任务已入队；静态 IP 与可选脚本状态将自动刷新。');
      await poller.refresh();
    } catch (error) { setSafeText(createMessage, `${LIGHTSAIL_SCRIPT_WARNING} ${error.message}`); }
    finally { submit.disabled = false; }
  });

  find('#lightsail-stop-cancel')?.addEventListener('click', () => stopDialog.close());
  find('#lightsail-stop-confirm')?.addEventListener('click', async event => {
    event.currentTarget.disabled = true;
    try { const request = actionRequest(selected, 'stop'); await api(request.path, request.body); stopDialog.close(); onMessage('关机任务已入队；静态 IP 将保留。'); await poller.refresh(); }
    catch (error) { setSafeText(stopMessage, error.message); }
    finally { event.currentTarget.disabled = false; }
  });
  find('#lightsail-destroy-cancel')?.addEventListener('click', () => destroyDialog.close());
  find('#lightsail-destroy-confirm')?.addEventListener('click', async event => {
    event.currentTarget.disabled = true;
    try { const request = actionRequest(selected, 'destroy', destroyInput.value); await api(request.path, request.body); destroyDialog.close(); onMessage('销毁任务已入队；实例删除后才会释放静态 IP。'); await poller.refresh(); }
    catch (error) { setSafeText(destroyMessage, error.message); }
    finally { event.currentTarget.disabled = false; }
  });

  return {
    async setActive(next) {
      active = Boolean(next);
      if (!active) { activationGate.stop(); poller.stop(); catalogLoader.cancel(); return; }
      const activation = activationGate.begin();
      showMessage('');
      try {
        if (await loadStatus(activation.isCurrent) && activation.isCurrent()) await poller.start();
        if (!activation.isCurrent()) poller.stop();
      } catch (error) { if (activation.isCurrent()) showMessage(error.message); }
    },
    refresh() { return active ? poller.refresh() : Promise.resolve(); },
    stop() { active = false; activationGate.stop(); poller.stop(); catalogLoader.cancel(); },
  };
}
