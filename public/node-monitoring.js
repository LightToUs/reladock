import { formatBytes, formatRate } from './telemetry.js';

function validDate(value) {
  const date = value ? new Date(value) : null;
  return date && Number.isFinite(date.valueOf()) ? date : null;
}

function elapsedLabel(startedAt, now) {
  const started = validDate(startedAt);
  if (!started || started > now) return '未知';
  const seconds = Math.floor((now - started) / 1000);
  if (seconds < 60) return '不足 1 分钟';
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时`;
  return `${Math.floor(seconds / 86400)} 天`;
}

function observedLabel(value) {
  const date = validDate(value);
  return date ? date.toLocaleString('zh-CN', { hour12: false }) : '未知';
}

function metric(value, state) {
  if (state === 'stale') return { value: null, label: '数据已过期' };
  if (!Number.isFinite(value)) return { value: null, label: '未知' };
  const rounded = Math.max(0, Math.min(100, Math.round(value)));
  return { value: rounded, label: `${rounded}%` };
}

function contactPresentation(status) {
  if (status === 'online') return { label: '在线', tone: 'online' };
  if (status === 'offline') return { label: '离线', tone: 'offline' };
  return { label: '尚未联系', tone: 'unknown' };
}

function trafficPresentation(node) {
  const hasReports = node.hasReports === true;
  const total = hasReports ? formatBytes(node.totalBytes) : '尚无报告';
  const uploadTotal = hasReports ? formatBytes(node.uploadBytes) : '尚无报告';
  const downloadTotal = hasReports ? formatBytes(node.downloadBytes) : '尚无报告';
  if (node.rateState === 'fresh' && Number.isFinite(node.uploadBytesPerSecond) && Number.isFinite(node.downloadBytesPerSecond)) {
    return {
      state: 'fresh',
      uploadRate: formatRate(node.uploadBytesPerSecond),
      downloadRate: formatRate(node.downloadBytesPerSecond),
      totalRate: Number.isFinite(node.totalBytesPerSecond) ? formatRate(node.totalBytesPerSecond) : '未知',
      uploadTotal, downloadTotal, total,
      detail: '最近有效计量报告的接收时间差值，不代表保证带宽。',
    };
  }
  const rateLabel = !hasReports || node.rateState === 'none'
    ? '尚无报告'
    : node.rateState === 'stale' ? '速率已过期' : '样本不足';
  return {
    state: !hasReports || node.rateState === 'none' ? 'none' : node.rateState === 'stale' ? 'stale' : 'insufficient',
    uploadRate: rateLabel,
    downloadRate: rateLabel,
    totalRate: rateLabel,
    uploadTotal, downloadTotal, total,
    detail: rateLabel === '速率已过期' ? '保留累计流量，不展示过期速率。' : '没有足够报告时不推断速率为 0。',
  };
}

export function nodeMonitoringPresentation(node = {}, nowInput = new Date()) {
  const now = validDate(nowInput) || new Date();
  const resourceObservedAt = validDate(node.resourceObservedAt);
  const resourceState = !resourceObservedAt
    ? 'unknown'
    : now - resourceObservedAt > 90_000 ? 'stale'
      : node.resourceStatus === 'pressure' || node.resourceStatus === 'drifted' ? node.resourceStatus : 'fresh';
  return {
    contact: contactPresentation(node.contactStatus),
    traffic: trafficPresentation(node),
    resources: {
      state: resourceState,
      observedAt: observedLabel(node.resourceObservedAt),
      cpu: metric(node.cpuPercent, resourceState),
      memory: metric(node.memoryPercent, resourceState),
      disk: metric(node.diskPercent, resourceState),
      conntrack: metric(node.conntrackPercent, resourceState),
    },
    agent: {
      runtime: elapsedLabel(node.agentStartedAt, now),
      startedAt: observedLabel(node.agentStartedAt),
      detail: 'Agent 本次进程运行时长，不是 VPS 开机时长。',
    },
    dataPlane: {
      state: node.dataPlaneStatus || 'unknown',
      detail: '配置一致性与 Agent 在线状态均不代表端到端转发健康。',
    },
    lastReportedAt: observedLabel(node.lastReportedAt),
  };
}

export function createAdaptivePoller({
  run,
  documentTarget = globalThis.document,
  activeMs = 5000,
  hiddenMs = 30000,
  setTimer = globalThis.setTimeout,
  clearTimer = globalThis.clearTimeout,
}) {
  let running = false;
  let timer = null;
  let inFlight = null;
  let refreshWhenSettled = false;

  const clearScheduled = () => {
    if (timer !== null) clearTimer(timer);
    timer = null;
  };
  const schedule = () => {
    clearScheduled();
    if (!running || inFlight) return;
    const delay = documentTarget?.hidden ? hiddenMs : activeMs;
    timer = setTimer(() => {
      timer = null;
      execute(false);
    }, delay);
  };
  const execute = queueWhenBusy => {
    if (!running) return Promise.resolve();
    if (inFlight) {
      if (queueWhenBusy) refreshWhenSettled = true;
      return inFlight;
    }
    clearScheduled();
    let result;
    try {
      result = run();
    } catch (error) {
      result = Promise.reject(error);
    }
    const request = Promise.resolve(result)
      .catch(() => undefined)
      .finally(() => {
        if (inFlight === request) inFlight = null;
        if (!running) return;
        if (refreshWhenSettled) {
          refreshWhenSettled = false;
          execute(false);
        } else {
          schedule();
        }
      });
    inFlight = request;
    return request;
  };
  const visibilityChanged = () => {
    clearScheduled();
    if (!running) return;
    if (documentTarget?.hidden) {
      if (!inFlight) schedule();
      return;
    }
    execute(true);
  };

  return {
    start() {
      if (running) return inFlight || Promise.resolve();
      running = true;
      documentTarget?.addEventListener?.('visibilitychange', visibilityChanged);
      return execute(false);
    },
    stop() {
      if (!running) return;
      running = false;
      refreshWhenSettled = false;
      clearScheduled();
      documentTarget?.removeEventListener?.('visibilitychange', visibilityChanged);
    },
    refresh() { return execute(false); },
  };
}
