const fieldNames = ['id', 'name', 'region', 'listenAddress', 'publicAddress', 'capacityMbps', 'resourceMode', 'dedicatedTenantId'];

export async function loadTenantDirectory(request) {
  const tenants = [];
  for (let page = 1; page <= 100000; page++) {
    const result = await request(`/api/admin/tenants?page=${page}`);
    if (!result || !Array.isArray(result.items) || !Number.isInteger(result.total) || !Number.isInteger(result.pageSize) || result.pageSize < 1) throw new Error('企业目录响应无效');
    tenants.push(...result.items);
    if (tenants.length >= result.total) return tenants;
  }
  throw new Error('企业目录分页超出限制');
}

export function filterTenantOptions(tenants, query) {
  const needle = query.trim().toLocaleLowerCase();
  return tenants.filter(tenant => !needle || [tenant.id, tenant.name, tenant.ownerEmail].some(value => String(value || '').toLocaleLowerCase().includes(needle))).slice(0, 20);
}

export async function loadTenantOptions({ query, request, loadDirectory = () => loadTenantDirectory(request) }) {
  try {
    return await request(`/api/admin/tenant-options?q=${encodeURIComponent(query)}`);
  } catch (error) {
    if (error.status !== 404) throw error;
  }
  return filterTenantOptions(await loadDirectory(), query);
}

export function bindNodeEditor({ dialog, form, title, save, status, createButton, cancelButton, searchTenants = () => {} }) {
  let editingNode = null;
  let session = 0;
  const resourceMode = form.elements.namedItem('resourceMode');
  const dedicatedTenant = form.elements.namedItem('dedicatedTenantId');

  function syncDedicatedTenant() {
    const shared = resourceMode.value === 'shared';
    dedicatedTenant.disabled = shared;
    dedicatedTenant.required = !shared;
    if (shared) dedicatedTenant.value = '';
    else searchTenants(dedicatedTenant.value.trim());
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
  dedicatedTenant.addEventListener('input', () => {
    if (!dedicatedTenant.disabled) searchTenants(dedicatedTenant.value.trim());
  });
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
