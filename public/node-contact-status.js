const presentations = {
  online: { label: '在线', tone: 'online', detail: '最近 90 秒收到心跳；不代表转发健康' },
  offline: { label: '离线', tone: 'offline', detail: '心跳已超时；Agent 会自动重连，尚未确认恢复' },
  'never-seen': { label: '待 Agent 连接', tone: 'pending', detail: '尚无 Agent 心跳记录' },
};

export function agentContactPresentation(status) {
  return presentations[status] || { label: '状态未知', tone: 'unknown', detail: '没有可确认的 Agent 联系状态' };
}

export function certificatePresentation(node, now = new Date()) {
  if (!node.certificateFingerprint) return { label: '未绑定', tone: 'unknown', detail: '没有节点证书' };
  const expires = node.certificateNotAfter ? new Date(node.certificateNotAfter) : null;
  const expiryDetail = expires && !Number.isNaN(expires.valueOf()) ? `有效期至 ${expires.toLocaleString()}` : 'Agent 尚未报告证书有效期';
  if (expires && !Number.isNaN(expires.valueOf()) && expires <= now && node.rotationState === 'error') return { label: '已过期 · 轮换失败', tone: 'offline', detail: `${expiryDetail}；错误代码 ${node.rotationErrorCode || 'unknown'}` };
  if (node.rotationState === 'error') return { label: '轮换失败', tone: 'offline', detail: `${expiryDetail}；错误代码 ${node.rotationErrorCode || 'unknown'}` };
  if (node.rotationState === 'pending') return { label: '轮换中', tone: 'pending', detail: `${expiryDetail}；等待激活确认` };
  if (!expires || Number.isNaN(expires.valueOf())) return { label: '已绑定 · 有效期未知', tone: 'unknown', detail: expiryDetail };
  if (expires <= now) return { label: '已过期', tone: 'offline', detail: expiryDetail };
  if (expires.valueOf() - now.valueOf() <= 7 * 86400000) return { label: '即将到期', tone: 'pending', detail: expiryDetail };
  return { label: '正常', tone: 'online', detail: expiryDetail };
}

export function configurationPresentation(node) {
  const detail = `期望 / 校验 / 应用：${node.desiredGeneration} / ${node.validatedGeneration} / ${node.appliedGeneration}；端到端健康未知`;
  if (node.appliedGeneration > 0 && node.appliedGeneration === node.desiredGeneration) return { label: '已应用 · 健康未知', detail };
  if (node.validatedGeneration === node.desiredGeneration) return { label: '校验通过 · 未应用', detail };
  return { label: '等待 Agent 确认', detail };
}

export function probePresentation(node) {
  const observed = node.probeObservedAt ? `；观测于 ${new Date(node.probeObservedAt).toLocaleString()}` : '';
  switch (node.probeStatus) {
    case 'targetReachable': return { label: '目标 TCP 可达', tone: 'online', detail: `仅表示入口节点到目标端口可达，不代表客户端端到端健康${observed}` };
    case 'targetUnreachable': return { label: '目标 TCP 不可达', tone: 'offline', detail: `入口节点探测失败；不是完整协议诊断${observed}` };
    case 'udpUnknown': return { label: 'UDP 健康未知', tone: 'unknown', detail: `普通 UDP 无通用握手，未推断端到端健康${observed}` };
    default: return { label: '未有当前探测', tone: 'unknown', detail: '没有与当前应用版本匹配的探测事实' };
  }
}

export function resourcePresentation(node) {
  const values = [];
  for (const [label, value] of [['CPU', node.cpuPercent], ['内存', node.memoryPercent], ['磁盘', node.diskPercent], ['连接跟踪', node.conntrackPercent]]) {
    if (Number.isInteger(value)) values.push(`${label} ${value}%`);
  }
  if (Number.isInteger(node.clockSkewSeconds)) values.push(`时钟偏差 ${node.clockSkewSeconds} 秒`);
  const observed = node.resourceObservedAt ? `；接收于 ${new Date(node.resourceObservedAt).toLocaleString()}` : '';
  const facts = values.length ? values.join(' / ') : '可用资源计数不足';
  const suffix = `；${facts}${observed}；不代表端到端健康`;
  switch (node.resourceStatus) {
    case 'normal': return { label: '资源正常', tone: 'online', detail: `最新固定阈值内${suffix}` };
    case 'pressure': return { label: '资源压力', tone: 'pending', detail: `至少一项达到告警阈值${suffix}` };
    case 'drifted': return { label: '转发配置漂移', tone: 'offline', detail: `本地只读校验与持久配置不一致${suffix}` };
    case 'stale': return { label: '资源遥测过期', tone: 'unknown', detail: `最近 90 秒没有新资源样本${suffix}` };
    default: return { label: '资源状态未知', tone: 'unknown', detail: `尚无足够的当前资源事实${suffix}` };
  }
}
