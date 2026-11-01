export function workspaceSummary({ rules = [], nodes = [] } = {}) {
  const appliedRules = rules.filter(rule => rule.appliedVersion > 0 && rule.appliedVersion === rule.desiredVersion).length;
  return {
    totalRules: rules.length,
    appliedRules,
    pendingRules: rules.length - appliedRules,
    contactedNodes: nodes.filter(node => Boolean(node.lastSeenAt)).length,
    totalNodes: nodes.length,
  };
}

export function ruleStatusPresentation(rule) {
  if (!rule.appliedVersion) {
    return rule.status === 'paused'
      ? { label: '待确认暂停', tone: 'pending', detail: '尚无节点应用记录' }
      : { label: '待 Agent 部署', tone: 'idle', detail: '尚未应用转发配置' };
  }
  if (rule.appliedVersion !== rule.desiredVersion) {
    return { label: '等待应用确认', tone: 'pending', detail: `生效版本 ${rule.appliedVersion} / 期望版本 ${rule.desiredVersion}` };
  }
  if (rule.appliedStatus === 'paused') {
    return { label: '暂停配置已应用', tone: 'paused', detail: '端口保留，转发已暂停' };
  }
  return { label: '配置已应用', tone: 'applied', detail: '端到端健康待验证' };
}
