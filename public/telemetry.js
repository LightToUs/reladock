const byteUnits = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
const rateUnits = ['bps', 'Kbps', 'Mbps', 'Gbps', 'Tbps'];

function finiteNonNegative(value) {
  return Number.isFinite(value) && value >= 0 ? value : 0;
}

export function formatBytes(value) {
  let amount = finiteNonNegative(Number(value));
  let index = 0;
  while (amount >= 1024 && index < byteUnits.length - 1) {
    amount /= 1024;
    index += 1;
  }
  if (index === 0) return `${Math.round(amount).toLocaleString()} B`;
  return `${amount.toFixed(2)} ${byteUnits[index]}`;
}

export function formatRate(bytesPerSecond) {
  let amount = finiteNonNegative(Number(bytesPerSecond)) * 8;
  let index = 0;
  while (amount >= 1000 && index < rateUnits.length - 1) {
    amount /= 1000;
    index += 1;
  }
  if (index === 0) return `${Math.round(amount).toLocaleString()} bps`;
  return `${amount.toFixed(2)} ${rateUnits[index]}`;
}

export function telemetryPresentation(summary = {}) {
  const hasReports = summary.hasReports === true;
  const base = {
    total: hasReports ? formatBytes(summary.totalBytes) : '尚未收到流量报告',
    upload: hasReports ? formatBytes(summary.uploadBytes) : '未知',
    download: hasReports ? formatBytes(summary.downloadBytes) : '未知',
    rate: '近期样本不足',
    uploadRate: '未知',
    downloadRate: '未知',
    utilization: '未知',
    rateTone: 'insufficient',
    rateDetail: '需要同一计量周期内至少两个有效报告；没有报告不推断为 0。',
  };

  if (summary.rateState === 'stale') {
    return { ...base, rate: '近期速率过期', rateTone: 'stale', rateDetail: '最近报告已超过 30 秒；保留累计流量，不再展示旧速率。' };
  }
  if (summary.rateState !== 'fresh' || !Number.isFinite(summary.totalBytesPerSecond)) return base;

  const seconds = Number.isFinite(summary.rateWindowSeconds) ? summary.rateWindowSeconds : 0;
  return {
    ...base,
    rate: formatRate(summary.totalBytesPerSecond),
    uploadRate: formatRate(summary.uploadBytesPerSecond),
    downloadRate: formatRate(summary.downloadBytesPerSecond),
    utilization: Number.isFinite(summary.utilizationBasisPoints) ? `${(summary.utilizationBasisPoints / 100).toFixed(2)}%` : '未知',
    rateTone: 'fresh',
    rateDetail: `最近 ${seconds} 秒报告差值，按平台接收时间估算；不是瞬时值或保证带宽。`,
  };
}

export function trendBars(points = [], bucketSeconds = 0) {
  const safe = Array.isArray(points) ? points.map(point => ({
    startedAt: point.startedAt,
    uploadBytes: finiteNonNegative(Number(point.uploadBytes)),
    downloadBytes: finiteNonNegative(Number(point.downloadBytes)),
    totalBytes: finiteNonNegative(Number(point.totalBytes)),
  })) : [];
  const maxBytes = safe.reduce((maximum, point) => Math.max(maximum, point.totalBytes), 0);
  const bucketMilliseconds = Number(bucketSeconds) * 1000;
  const firstTime = safe.length ? new Date(safe[0].startedAt).valueOf() : 0;
  let lastSlot = -1;
  const bars = safe.map((point, index) => {
    const pointTime = new Date(point.startedAt).valueOf();
    const calculated = bucketMilliseconds > 0 && Number.isFinite(pointTime) && Number.isFinite(firstTime)
      ? Math.max(0, Math.round((pointTime - firstTime) / bucketMilliseconds))
      : index;
    const slot = Math.max(lastSlot + 1, calculated);
    lastSlot = slot;
    return { ...point, ratio: maxBytes > 0 ? point.totalBytes / maxBytes : 0, slot };
  });
  return {
    maxBytes,
    slotCount: lastSlot + 1,
    bars,
  };
}

export function createLatestRequestGate() {
  let current = 0;
  return {
    next() { current += 1; return current; },
    current() { return current; },
    isCurrent(token) { return token === current; },
    invalidate() { current += 1; },
  };
}

export function telemetryInteractionState(available, ruleID) {
  const historyAllowed = available === true && typeof ruleID === 'string' && ruleID.length > 0;
  return { controlsDisabled: !historyAllowed, historyAllowed };
}

export async function loadWorkspaceData(api) {
  const overviewRequest = api('/api/overview');
  const optionalRequest = Promise.allSettled([
    api('/api/billing/telemetry'),
    api('/api/operations/nodes?staleAfterSeconds=90'),
  ]);
  const overview = await overviewRequest;
  const [telemetryResult, nodesResult] = await optionalRequest;
  for (const result of [telemetryResult, nodesResult]) {
    if (result.status === 'rejected' && result.reason?.status === 401) throw result.reason;
  }
  const errors = [];
  if (telemetryResult.status === 'rejected') errors.push({ path: '/api/billing/telemetry', error: telemetryResult.reason });
  if (nodesResult.status === 'rejected') errors.push({ path: '/api/operations/nodes', error: nodesResult.reason });
  return {
    overview,
    telemetry: telemetryResult.status === 'fulfilled' ? telemetryResult.value : [],
    operationNodes: nodesResult.status === 'fulfilled' ? (nodesResult.value.items || []) : [],
    telemetryAvailable: telemetryResult.status === 'fulfilled',
    resourcesAvailable: nodesResult.status === 'fulfilled',
    errors,
  };
}
