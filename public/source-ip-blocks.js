export function sourceBlockCIDR(value) {
  const parts = String(value).split('.');
  if (parts.length !== 4 || parts.some(part => !/^\d{1,3}$/.test(part) || Number(part) > 255 || String(Number(part)) !== part)) {
    throw new Error('仅支持标准 IPv4 地址');
  }
  return `${parts.join('.')}/32`;
}

export function sourceBlockRequestForFlow(flow, desiredVersion) {
  return {
    path: `/api/admin/rules/${encodeURIComponent(flow.ruleId)}/source-blocks`,
    body: {
      tenantId: flow.tenantId,
      desiredVersion,
      cidr: sourceBlockCIDR(flow.sourceIp),
      reason: `来源连接记录 ${flow.eventId}`,
    },
  };
}

export function sourceBlockPresentation(block) {
  if (block.state === 'removed') {
    return block.applied
      ? { label: '节点已移除', detail: '应用确认不代表端到端健康' }
      : { label: '等待节点移除', detail: `移除版本 ${block.removedVersion}；节点仍可能执行此拉黑` };
  }
  return block.applied
    ? { label: '节点已应用', detail: '应用确认不代表端到端健康' }
    : { label: '等待节点应用', detail: `请求版本 ${block.requestedVersion}；尚未生效` };
}

export async function runCommittedSourceBlockMutation({ mutate, refresh }) {
  const result = await mutate();
  try {
    await refresh();
  } catch (error) {
    error.committed = true;
    throw error;
  }
  return result;
}

