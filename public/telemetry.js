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

export function trendBars(points = []) {
  const safe = Array.isArray(points) ? points.map(point => ({
    startedAt: point.startedAt,
    uploadBytes: finiteNonNegative(Number(point.uploadBytes)),
    downloadBytes: finiteNonNegative(Number(point.downloadBytes)),
    totalBytes: finiteNonNegative(Number(point.totalBytes)),
  })) : [];
  const maxBytes = safe.reduce((maximum, point) => Math.max(maximum, point.totalBytes), 0);
  return {
    maxBytes,
    bars: safe.map(point => ({ ...point, ratio: maxBytes > 0 ? point.totalBytes / maxBytes : 0 })),
  };
}
