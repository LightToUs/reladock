const labels = {
  shared: '共享资源',
  'dedicated-tenant': '企业专属资源',
  'dedicated-rule': '规则专属资源',
};

export function resourceModeLabel(value) {
  return labels[value] || `未知资源模式（${value || '空'}）`;
}

export function planMatchesNode(plan, node) {
  return Boolean(plan?.resourceMode && node?.resourceMode && plan.resourceMode === node.resourceMode);
}
