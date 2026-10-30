const presentations = {
  online: { label: '在线', tone: 'online' },
  offline: { label: '离线', tone: 'offline' },
  'never-seen': { label: '待 Agent 连接', tone: 'pending' },
};

export function agentContactPresentation(status) {
  return presentations[status] || { label: '状态未知', tone: 'unknown' };
}
