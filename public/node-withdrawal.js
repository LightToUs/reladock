export function nodeWithdrawalControl(node) {
  if (!node.withdrawable) return null;
  return {
    label: '撤销登记',
    path: `/api/admin/nodes/${encodeURIComponent(node.id)}/withdraw-registration`,
    body: { revision: node.revision },
    confirmText: `撤销“${node.name}”的节点登记？仅从未部署且没有任何业务或运行历史的空节点可以撤销；此操作不可恢复。`,
    successText: '未部署的空节点登记已撤销。',
    danger: true,
  };
}
