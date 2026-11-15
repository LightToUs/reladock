export function sourceLoggingPresentation(rule) {
  const requested = Boolean(rule.sourceLoggingEnabled);
  const applied = Boolean(rule.sourceLoggingAppliedEnabled);
  if (!requested && !applied) return { key: 'disabled', label: '源地址记录：关闭' };
  if (rule.sourceLoggingUnsupported) return { key: 'unsupported', label: '源地址记录：节点不支持' };
  if (requested && !applied) return { key: 'waiting-enable', label: '源地址记录：等待启用确认' };
  if (!requested && applied) return { key: 'waiting-disable', label: '源地址记录：等待关闭确认' };
  if (rule.sourceLoggingCollectorStatus === 'degraded' || rule.sourceLoggingCollectorStatus === 'unavailable') return { key: 'degraded', label: '源地址记录：采集不完整' };
  return { key: 'enabled', label: '源地址记录：已启用' };
}

export function sourceLoggingConfirmation(enabled) {
  return enabled
    ? '启用后仅记录新连接的来源 IP、来源端口和规则归属，不记录包体、URL、DNS 或 SNI。该功能会增加节点与数据库开销，记录按管理员配置的保留期清理；启用仍需等待 Agent 应用确认。确认启用？'
    : '关闭后需等待 Agent 应用确认才停止新记录；已有记录仍按保留期清理。确认关闭？';
}
