export function nodeActionDescriptor(kind, node) {
  if (kind === 'withdraw-registration' && !node.withdrawable) return null;
  if (kind === 'rotate-certificate' && (!node.certificateFingerprint || !node.certificateRotationSupported || node.certificateRotationRequested || ['draining', 'retired'].includes(node.status))) return null;
  const definitions = {
    'rotate-certificate': {
      title: '自动更换证书', confirmLabel: '请求更换', danger: false,
      description: 'Agent 将自动生成新私钥并申请证书，激活后替换旧身份，无需手动填写证书或重新安装。节点离线时等待恢复连接。',
      successText: '更换请求已提交，等待 Agent 完成证书激活。',
    },
    maintenance: {
      title: '进入维护', confirmLabel: '确认进入维护', danger: false,
      description: '进入维护只停止新增端口分配，不会停止已有业务流量。',
      successText: '节点已进入维护，新的端口分配已停止。',
    },
    enable: {
      title: '恢复分配', confirmLabel: '确认恢复分配', danger: false,
      description: '恢复后允许新规则分配到此节点；这不代表节点、目标或端到端链路健康。',
      successText: '节点已恢复新的端口分配。',
    },
    retry: {
      title: '重试配置交付', confirmLabel: '生成并重新交付', danger: false,
      description: '平台将生成最新完整配置快照，等待 Agent 领取。应用确认仍不代表端到端健康。',
      successText: '已生成最新完整快照，等待 Agent 领取。',
    },
    upgrade: {
      title: '升级 Agent', confirmLabel: '创建升级任务', danger: false,
      description: '平台只使用已配置的固定版本。升级成功仅证明控制连接与配置轮询恢复，不代表实际转发健康。',
      successText: '升级任务已创建，等待节点领取。',
    },
    'upgrade-cancel': {
      title: '取消 Agent 升级', confirmLabel: '确认取消升级', danger: false,
      description: '只能取消尚未开始安装的升级任务；已进入安装或验证阶段的任务不会被强制中断。',
      successText: '升级任务已取消。',
    },
    'start-draining': {
      title: '进入安全排空', confirmLabel: '确认进入安全排空', danger: true,
      description: '排空一旦开始就不可恢复分配。请继续删除全部规则，并等待端口、配置与 Agent 证明全部收敛。',
      successText: '节点已进入排空；等待规则、端口、配置与 Agent 证明全部收敛。',
    },
    'finalize-retirement': {
      title: '最终退役节点', confirmLabel: '确认最终退役', danger: true,
      description: '最终退役会撤销 Agent 身份，节点不可恢复；历史记录与端口隔离证据仍会保留。',
      successText: '节点已最终退役，Agent 身份已撤销。',
    },
    'withdraw-registration': {
      title: '撤销节点登记', confirmLabel: '确认撤销登记', danger: true,
      description: '仅从未部署且没有任何业务或运行历史的空节点可以撤销登记；此操作不可恢复。',
      successText: '未部署的空节点登记已撤销。',
    },
    'revoke-certificate': {
      title: '撤销节点证书', confirmLabel: '确认撤销证书', danger: true,
      description: '撤销当前证书的 Agent 接入权限，不会停止已有数据面流量。',
      successText: '节点证书授权已撤销。',
    },
    hide: {
      title: '隐藏节点', confirmLabel: '确认隐藏', danger: false,
      description: '仅从默认入口节点列表隐藏；不删除客户规则，不改变 Agent、转发、计费或安全退役状态。',
      successText: '节点已从默认列表隐藏，可通过“显示已隐藏节点”恢复查看。',
    },
    unhide: {
      title: '恢复显示', confirmLabel: '确认恢复显示', danger: false,
      description: '恢复到默认入口节点列表；不会改变节点运行或安全退役状态。',
      successText: '节点已恢复到默认列表。',
    },
  };
  const definition = definitions[kind];
  if (!definition) return null;
  const visibility = kind === 'hide' || kind === 'unhide';
  return {
    ...definition,
    title: `${definition.title} · ${node.name}`,
    path: `/api/admin/nodes/${encodeURIComponent(node.id)}/${visibility ? 'visibility' : kind}`,
    body: visibility ? { visibilityRevision: node.visibilityRevision, hidden: kind === 'hide' } : { revision: node.revision },
  };
}

export function createNodeActionRunner({ kind, node, getNode, mutate, conflictMessage, onRefresh = () => {}, onSuccess = () => {} }) {
  let representedNode = node;
  return async () => {
    const action = nodeActionDescriptor(kind, representedNode);
    if (!action) throw new Error('当前节点状态已不允许此操作，请刷新后重试。');
    const saved = await mutate(action.path, action.body, action.successText);
    if (saved) { await onSuccess(); return; }
    const current = getNode(node.id);
    const refreshedAction = current && nodeActionDescriptor(kind, current);
    if (!refreshedAction) throw new Error('当前节点状态已不允许此操作，请刷新后重试。');
    representedNode = current;
    onRefresh(refreshedAction);
    throw new Error(conflictMessage() || '操作冲突，请检查最新状态后重新确认。');
  };
}

export function bindNodeActionDialog({ dialog, title, description, status, confirmButton, cancelButton }) {
  let action = null;
  let busy = false;

  function setBusy(value) {
    busy = value;
    confirmButton.disabled = value;
    cancelButton.disabled = value;
  }

  function close() {
    if (dialog.open) dialog.close();
  }

  async function confirm() {
    if (!action || busy) return;
    const active = action;
    setBusy(true);
    status.textContent = '正在处理…';
    try {
      await active.run();
      if (action === active) close();
    } catch (error) {
      if (action === active) status.textContent = error.message || '操作失败';
    } finally {
      if (dialog.open && action === active) setBusy(false);
    }
  }

  function open(nextAction) {
    action = nextAction;
    present(nextAction);
    status.textContent = '';
    setBusy(false);
    if (!dialog.open) dialog.showModal();
  }

  function present(nextAction) {
    title.textContent = nextAction.title;
    description.textContent = nextAction.description;
    confirmButton.textContent = nextAction.confirmLabel;
    confirmButton.classList.toggle('danger', Boolean(nextAction.danger));
  }

  function update(nextAction) {
    if (!action) return;
    const run = action.run;
    Object.assign(action, nextAction, { run });
    present(action);
  }

  confirmButton.addEventListener('click', confirm);
  cancelButton.addEventListener('click', () => { if (!busy) close(); });
  dialog.addEventListener('cancel', event => { if (busy) event.preventDefault(); });
  dialog.addEventListener('close', () => {
    action = null;
    status.textContent = '';
    setBusy(false);
  });

  return { open, update, close };
}
