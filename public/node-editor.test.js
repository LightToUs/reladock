import assert from 'node:assert/strict';
import test from 'node:test';

import { bindNodeEditor } from './node-editor.js';

class Control extends EventTarget {
  click() { this.dispatchEvent(new Event('click')); }
}

class Field extends EventTarget {
  constructor(value) {
    super();
    this.value = value;
    this.initialValue = value;
    this.readOnly = false;
    this.disabled = false;
    this.required = false;
    this.focused = false;
  }
  focus() { this.focused = true; }
  change() { this.dispatchEvent(new Event('change')); }
}

function fixture() {
  const fields = Object.fromEntries([
    ['id', ''],
    ['name', ''],
    ['region', ''],
    ['listenAddress', '0.0.0.0'],
    ['publicAddress', ''],
    ['capacityMbps', '1000'],
    ['resourceMode', 'shared'],
    ['dedicatedTenantId', ''],
  ].map(([name, initialValue]) => [name, new Field(initialValue)]));
  const form = {
    elements: { namedItem: name => fields[name] },
    reset() { for (const field of Object.values(fields)) field.value = field.initialValue; },
  };
  const dialog = new EventTarget();
  dialog.open = false;
  dialog.showModal = () => { dialog.open = true; };
  dialog.close = () => { dialog.open = false; dialog.dispatchEvent(new Event('close')); };
  return {
    fields,
    form,
    dialog,
    title: { textContent: '' },
    save: { textContent: '' },
    status: { textContent: '旧错误' },
    createButton: new Control(),
    cancelButton: new Control(),
  };
}

test('new-node action opens a reset create dialog with editable identity', () => {
  const ui = fixture();
  ui.fields.name.value = '旧草稿';
  const editor = bindNodeEditor(ui);

  ui.createButton.click();

  assert.equal(ui.dialog.open, true);
  assert.equal(editor.current(), null);
  assert.equal(ui.title.textContent, '新增入口节点');
  assert.equal(ui.save.textContent, '登记节点');
  assert.equal(ui.fields.id.readOnly, false);
  assert.equal(ui.fields.name.value, '');
  assert.equal(ui.fields.listenAddress.value, '0.0.0.0');
  assert.equal(ui.fields.capacityMbps.value, '1000');
	assert.equal(ui.fields.resourceMode.value, 'shared');
	assert.equal(ui.fields.dedicatedTenantId.value, '');
	assert.equal(ui.fields.dedicatedTenantId.disabled, true);
	assert.equal(ui.fields.dedicatedTenantId.required, false);
  assert.equal(ui.fields.id.focused, true);
  assert.equal(ui.status.textContent, '');
});

test('edit action opens the same dialog with node values and locked identity', () => {
  const ui = fixture();
  const editor = bindNodeEditor(ui);
  const node = {
    id: 'hk-02',
    name: '香港入口 02',
    region: 'Hong Kong',
    listenAddress: '0.0.0.0',
    publicAddress: '203.0.113.20',
    capacityMbps: 800,
	resourceMode: 'dedicated-tenant',
	dedicatedTenantId: '9bd89fc7-b8ce-4b79-904f-e71bd4540751',
    revision: 7,
  };

  editor.openEdit(node);

  assert.equal(ui.dialog.open, true);
  assert.equal(editor.current(), node);
  assert.equal(ui.title.textContent, '编辑节点 · 香港入口 02');
  assert.equal(ui.save.textContent, '保存节点修改');
  assert.equal(ui.fields.id.value, 'hk-02');
  assert.equal(ui.fields.capacityMbps.value, 800);
	assert.equal(ui.fields.resourceMode.value, 'dedicated-tenant');
	assert.equal(ui.fields.dedicatedTenantId.value, '9bd89fc7-b8ce-4b79-904f-e71bd4540751');
	assert.equal(ui.fields.dedicatedTenantId.disabled, false);
	assert.equal(ui.fields.dedicatedTenantId.required, true);
  assert.equal(ui.fields.id.readOnly, true);
  assert.equal(ui.fields.name.focused, true);
  assert.equal(ui.status.textContent, '');
});

test('switching to shared clears a previous dedicated tenant before create', () => {
  const ui = fixture();
  const editor = bindNodeEditor(ui);
  editor.openEdit({
    id: 'tenant-01',
    name: '企业专属',
    resourceMode: 'dedicated-rule',
    dedicatedTenantId: '9bd89fc7-b8ce-4b79-904f-e71bd4540751',
  });

  ui.fields.resourceMode.value = 'shared';
  ui.fields.resourceMode.change();

  assert.equal(ui.fields.dedicatedTenantId.value, '');
  assert.equal(ui.fields.dedicatedTenantId.disabled, true);
  assert.equal(ui.fields.dedicatedTenantId.required, false);

  editor.close();
  editor.openCreate();
  assert.equal(ui.fields.resourceMode.value, 'shared');
  assert.equal(ui.fields.dedicatedTenantId.value, '');
});

test('cancel closes the dialog and clears the edited node', () => {
  const ui = fixture();
  const editor = bindNodeEditor(ui);
  editor.openEdit({ id: 'hk-02', name: '香港入口 02' });

  ui.cancelButton.click();

  assert.equal(ui.dialog.open, false);
  assert.equal(editor.current(), null);
});

test('opening another node invalidates the previous dialog session', () => {
  const ui = fixture();
  const editor = bindNodeEditor(ui);
  editor.openEdit({ id: 'hk-02', name: '香港入口 02' });
  const firstSession = editor.session();

  editor.close();
  editor.openEdit({ id: 'sg-01', name: '新加坡入口 01' });

  assert.equal(editor.isCurrent(firstSession), false);
  assert.equal(editor.isCurrent(editor.session()), true);
  assert.equal(editor.current().id, 'sg-01');
  assert.equal(ui.save.disabled, false);
});

test('closing and reopening the same node creates a new editor session', () => {
  const ui = fixture();
  const editor = bindNodeEditor(ui);
  editor.openEdit({ id: 'hk-02', name: '旧名称', revision: 7 });
  const firstSession = editor.session();

  editor.close();
  editor.openEdit({ id: 'hk-02', name: '新草稿', revision: 8 });

  assert.equal(editor.isCurrent(firstSession), false);
  assert.equal(editor.current().name, '新草稿');
});
