const editableFields = ['name', 'protocol', 'targetHost', 'targetPort', 'bandwidthMbps', 'nodeId', 'desiredVersion', 'listenPort'];
const protocolLabels = { tcp: 'TCP', udp: 'UDP', both: 'TCP + UDP' };

export function createRuleEditor(dialog, form, elements) {
  let editingRule = null;
  let session = 0;
  const field = name => form.elements.namedItem(name);

  function show() {
    if (!dialog.open) dialog.showModal();
  }

  function setAllocationLocked(locked) {
    field('protocol').disabled = locked;
    field('nodeId').disabled = locked;
  }

  function openCreate() {
    session++;
    editingRule = null;
    form.reset();
    setAllocationLocked(false);
    elements.title.textContent = '创建中转规则';
    elements.description.textContent = '使用已购买套餐预留 TCP/UDP 入口端口，保存配置并等待节点交付。';
    elements.submit.textContent = '预留端口并保存';
    elements.submit.disabled = false;
    elements.status.textContent = '';
    elements.allocation.hidden = true;
    elements.allocation.textContent = '';
    show();
    field('name').focus();
  }

  function openEdit(rule) {
    session++;
    editingRule = rule;
    for (const name of editableFields) field(name).value = rule[name] ?? '';
    setAllocationLocked(true);
    elements.title.textContent = `编辑规则 · ${rule.name}`;
    elements.description.textContent = '修改目标与本规则带宽后，将生成新的期望配置版本并等待 Agent 确认。';
    elements.submit.textContent = '保存规则修改';
    elements.submit.disabled = false;
    elements.status.textContent = '';
    elements.allocation.hidden = false;
    elements.allocation.textContent = `入口 ${String(rule.nodeId).toUpperCase()} · ${protocolLabels[rule.protocol] || String(rule.protocol).toUpperCase()} · 端口 ${rule.listenPort}`;
    show();
    field('name').focus();
  }

  function close() {
    if (dialog.open) dialog.close();
    else editingRule = null;
  }

  elements.createButton.addEventListener('click', openCreate);
  elements.cancelButton.addEventListener('click', close);
  dialog.addEventListener('cancel', event => {
    event.preventDefault();
    close();
  });
  dialog.addEventListener('close', () => {
    session++;
    editingRule = null;
    elements.status.textContent = '';
  });

  return {
    openCreate,
    openEdit,
    close,
    current: () => editingRule,
    session: () => session,
    isCurrent: token => dialog.open && token === session,
  };
}
