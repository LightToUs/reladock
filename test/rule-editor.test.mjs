import assert from 'node:assert/strict';
import test from 'node:test';

import { createRuleEditor } from '../public/rule-editor.js';

class Control extends EventTarget {
  click() { this.dispatchEvent(new Event('click')); }
}

function fixture() {
  const initial = {
    name: '', protocol: 'udp', targetHost: '', targetPort: '443', bandwidthMbps: '1000',
    nodeId: 'hk-01', desiredVersion: '', listenPort: '',
  };
  const fields = Object.fromEntries(Object.entries(initial).map(([name, value]) => [name, {
    value, initialValue: value, disabled: false, focused: false,
    focus() { this.focused = true; },
  }]));
  const form = {
    elements: { namedItem: name => fields[name] },
    reset() { for (const field of Object.values(fields)) field.value = field.initialValue; },
  };
  const dialog = new EventTarget();
  dialog.open = false;
  dialog.showModal = () => { dialog.open = true; };
  dialog.close = () => { dialog.open = false; dialog.dispatchEvent(new Event('close')); };
  const elements = {
    title: { textContent: '' }, description: { textContent: '' }, submit: { textContent: '', disabled: true },
    status: { textContent: '旧提示' }, allocation: { hidden: true, textContent: '' },
    createButton: new Control(), cancelButton: new Control(),
  };
  return { fields, form, dialog, elements };
}

test('create action opens a clean dialog with allocation fields editable', () => {
  const ui = fixture();
  ui.fields.name.value = '旧草稿';
  const editor = createRuleEditor(ui.dialog, ui.form, ui.elements);

  ui.elements.createButton.click();

  assert.equal(ui.dialog.open, true);
  assert.equal(editor.current(), null);
  assert.equal(ui.elements.title.textContent, '创建中转规则');
  assert.equal(ui.elements.submit.textContent, '预留端口并保存');
  assert.equal(ui.elements.submit.disabled, false);
  assert.equal(ui.fields.name.value, '');
  assert.equal(ui.fields.targetPort.value, '443');
  assert.equal(ui.fields.protocol.disabled, false);
  assert.equal(ui.fields.nodeId.disabled, false);
  assert.equal(ui.elements.allocation.hidden, true);
  assert.equal(ui.fields.name.focused, true);
  assert.equal(ui.elements.status.textContent, '');
});
test('edit action fills mutable values and presents allocation identity as locked', () => {
  const ui = fixture();
  const editor = createRuleEditor(ui.dialog, ui.form, ui.elements);
  const rule = {
    id: 'rule-1', name: '新加坡落地 A', protocol: 'both', targetHost: '192.0.2.10',
    targetPort: 8443, bandwidthMbps: 500, nodeId: 'hk-01', listenPort: 20000, desiredVersion: 7,
  };

  editor.openEdit(rule);

  assert.equal(editor.current(), rule);
  assert.equal(ui.elements.title.textContent, '编辑规则 · 新加坡落地 A');
  assert.equal(ui.elements.submit.textContent, '保存规则修改');
  assert.equal(ui.fields.name.value, '新加坡落地 A');
  assert.equal(ui.fields.targetPort.value, 8443);
  assert.equal(ui.fields.desiredVersion.value, 7);
  assert.equal(ui.fields.protocol.disabled, true);
  assert.equal(ui.fields.nodeId.disabled, true);
  assert.equal(ui.elements.allocation.hidden, false);
  assert.equal(ui.elements.allocation.textContent, '入口 HK-01 · TCP + UDP · 端口 20000');
});

test('cancel clears edit mode and invalidates an older submission session', () => {
  const ui = fixture();
  const editor = createRuleEditor(ui.dialog, ui.form, ui.elements);
  editor.openEdit({ id: 'rule-1', name: 'A', protocol: 'tcp', nodeId: 'hk-01', listenPort: 20000 });
  const oldSession = editor.session();

  ui.elements.cancelButton.click();
  editor.openCreate();

  assert.equal(ui.dialog.open, true);
  assert.equal(editor.current(), null);
  assert.equal(editor.isCurrent(oldSession), false);
  assert.equal(editor.isCurrent(editor.session()), true);
});
