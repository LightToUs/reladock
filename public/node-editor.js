const fieldNames = ['id', 'name', 'region', 'listenAddress', 'publicAddress', 'capacityMbps', 'resourceMode', 'dedicatedTenantId'];

export function bindNodeEditor({ dialog, form, title, save, status, createButton, cancelButton }) {
  let editingNode = null;
  let session = 0;
  const resourceMode = form.elements.namedItem('resourceMode');
  const dedicatedTenant = form.elements.namedItem('dedicatedTenantId');

  function syncDedicatedTenant() {
    const shared = resourceMode.value === 'shared';
    dedicatedTenant.disabled = shared;
    dedicatedTenant.required = !shared;
    if (shared) dedicatedTenant.value = '';
  }

  function show() {
    if (!dialog.open) dialog.showModal();
  }

  function openCreate() {
    session++;
    editingNode = null;
    status.textContent = '';
    save.disabled = false;
    form.reset();
    syncDedicatedTenant();
    form.elements.namedItem('id').readOnly = false;
    title.textContent = '新增入口节点';
    save.textContent = '登记节点';
    show();
    form.elements.namedItem('id').focus();
  }

  function openEdit(node) {
    if (!dialog.open || editingNode?.id !== node.id) session++;
    editingNode = node;
    status.textContent = '';
    save.disabled = false;
    for (const name of fieldNames) form.elements.namedItem(name).value = node[name] ?? '';
    syncDedicatedTenant();
    form.elements.namedItem('id').readOnly = true;
    title.textContent = `编辑节点 · ${node.name}`;
    save.textContent = '保存节点修改';
    show();
    form.elements.namedItem('name').focus();
  }

  function close() {
    if (dialog.open) dialog.close();
    else editingNode = null;
  }

  createButton.addEventListener('click', openCreate);
  cancelButton.addEventListener('click', close);
  resourceMode.addEventListener('change', syncDedicatedTenant);
  dialog.addEventListener('close', () => { session++; editingNode = null; });
  syncDedicatedTenant();

  return {
    current: () => editingNode,
    session: () => session,
    isCurrent: token => dialog.open && token === session,
    openCreate,
    openEdit,
    close,
  };
}
