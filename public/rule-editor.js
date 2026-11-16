const editableFields = ['name', 'protocol', 'targetHost', 'targetPort', 'bandwidthMbps', 'nodeId', 'desiredVersion', 'listenPort', 'listenPortEnd'];
const protocolLabels = { tcp: 'TCP', udp: 'UDP', both: 'TCP + UDP' };

function rangeLabel(rule) {
  return rule.listenPortEnd && rule.listenPortEnd !== rule.listenPort
    ? `${rule.listenPort}–${rule.listenPortEnd}`
    : String(rule.listenPort);
}

export function createRuleEditor(dialog, form, elements) {
  let editingRule = null;
  let session = 0;
  let resourceMode = '';
  const field = name => form.elements.namedItem(name);

  function syncPortControls(nextResourceMode = resourceMode, resetAllocation = false) {
    resourceMode = nextResourceMode || '';
    const eligible = !editingRule && resourceMode === 'dedicated-tenant';
    const mode = field('portMode');
    if (resetAllocation && !editingRule) {
      mode.value = 'auto';
      field('listenPort').value = '';
      field('listenPortEnd').value = '';
    }
    mode.disabled = !eligible;
    if (!eligible) mode.value = 'auto';
    const custom = eligible && mode.value === 'custom';
    field('listenPort').disabled = !custom;
    field('listenPortEnd').disabled = !custom;
    field('listenPort').required = custom;
    field('listenPortEnd').required = false;
    if (!custom) {
      field('listenPort').value = '';
      field('listenPortEnd').value = '';
    }
    if (editingRule) elements.portHint.textContent = '入口端口在创建后锁定；如需更换端口，请创建新规则。';
    else if (!eligible) elements.portHint.textContent = '仅企业专属资源套餐支持自定义入口端口；当前套餐使用自动分配。';
    else if (custom) elements.portHint.textContent = '结束端口可留空表示单端口；连续端口段最多 4096 个端口。';
    else elements.portHint.textContent = '企业专属资源可选择自动分配，或切换为自定义端口。';
  }

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
    syncPortControls(elements.getResourceMode?.() || '');
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
    field('portMode').value = rule.listenPortEnd && rule.listenPortEnd !== rule.listenPort ? 'custom' : 'auto';
    setAllocationLocked(true);
    syncPortControls('');
    elements.title.textContent = `编辑规则 · ${rule.name}`;
    elements.description.textContent = '修改目标与本规则带宽后，将生成新的期望配置版本并等待 Agent 确认。';
    elements.submit.textContent = '保存规则修改';
    elements.submit.disabled = false;
    elements.status.textContent = '';
    elements.allocation.hidden = false;
    elements.allocation.textContent = `入口 ${String(rule.nodeId).toUpperCase()} · ${protocolLabels[rule.protocol] || String(rule.protocol).toUpperCase()} · 端口 ${rangeLabel(rule)}`;
    show();
    field('name').focus();
  }

  function close() {
    if (dialog.open) dialog.close();
    else editingRule = null;
  }

  elements.createButton.addEventListener('click', openCreate);
  elements.cancelButton.addEventListener('click', close);
  field('portMode').addEventListener?.('change', () => syncPortControls());
  field('protocol').addEventListener?.('change', () => syncPortControls(resourceMode, true));
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
    syncPortControls,
  };
}
