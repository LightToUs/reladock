import assert from 'node:assert/strict';
import test from 'node:test';

import { bindNodeContentDialog, bindNodeDeliveryDialog, runDialogSession } from './node-content-dialog.js';

class Control extends EventTarget {
  click() { this.dispatchEvent(new Event('click')); }
}

function dialogFixture() {
  const dialog = new EventTarget();
  dialog.open = false;
  dialog.showModal = () => { dialog.open = true; };
  dialog.close = () => { dialog.open = false; dialog.dispatchEvent(new Event('close')); };
  return dialog;
}

const flush = () => new Promise(resolve => setImmediate(resolve));

test('content dialogs expose the active node and clear it on every close path', () => {
  const dialog = dialogFixture();
  const closeButton = new Control();
  const closed = [];
  const controller = bindNodeContentDialog({ dialog, closeButton, onClose: node => closed.push(node?.id) });

  controller.open({ id: 'hk-02' });
  assert.equal(dialog.open, true);
  assert.equal(controller.current().id, 'hk-02');

  closeButton.click();
  assert.equal(dialog.open, false);
  assert.equal(controller.current(), null);
  assert.deepEqual(closed, ['hk-02']);

  controller.open({ id: 'sg-01' });
  const cancel = new Event('cancel', { cancelable: true });
  dialog.dispatchEvent(cancel);
  assert.equal(cancel.defaultPrevented, true);
  assert.equal(dialog.open, false);
  assert.equal(controller.current(), null);
  assert.deepEqual(closed, ['hk-02', 'sg-01']);
});

test('delivery dialog ignores an older response after another node is opened', async () => {
  const dialog = dialogFixture();
  const closeButton = new Control();
  const title = { textContent: '' };
  const status = { textContent: '' };
  const pending = new Map();
  const rendered = [];
  const controller = bindNodeDeliveryDialog({
    dialog,
    closeButton,
    title,
    status,
    load: id => new Promise((resolve, reject) => pending.set(id, { resolve, reject })),
    render: records => rendered.push(records),
  });

  const first = controller.open({ id: 'hk-02', name: '香港入口 02' });
  const second = controller.open({ id: 'sg-01', name: '新加坡入口 01' });
  assert.equal(title.textContent, '交付记录 · 新加坡入口 01');

  pending.get('sg-01').resolve([{ generation: 9 }]);
  await second;
  pending.get('hk-02').resolve([{ generation: 4 }]);
  await first;

  assert.deepEqual(rendered, [[], [], [{ generation: 9 }]]);
  assert.equal(status.textContent, '最近 1 份配置交付记录。');
});

test('delivery load errors stay inside the open dialog and closing invalidates the request', async () => {
  const dialog = dialogFixture();
  const closeButton = new Control();
  const status = { textContent: '' };
  let rejectRequest;
  const controller = bindNodeDeliveryDialog({
    dialog,
    closeButton,
    title: { textContent: '' },
    status,
    load: () => new Promise((resolve, reject) => { rejectRequest = reject; }),
    render: () => {},
  });

  const request = controller.open({ id: 'hk-02', name: '香港入口 02' });
  closeButton.click();
  rejectRequest(new Error('网络暂时不可用'));
  await request;
  await flush();

  assert.equal(dialog.open, false);
  assert.equal(status.textContent, '');
});

test('a stale dialog submission cannot close or write into a newly opened node', async () => {
  const dialog = dialogFixture();
  const controller = bindNodeContentDialog({ dialog, closeButton: new Control() });
  let resolveRequest;
  const effects = [];
  controller.open({ id: 'hk-02' });
  const request = runDialogSession(controller, () => new Promise(resolve => { resolveRequest = resolve; }), {
    success: () => effects.push('success'),
    error: () => effects.push('error'),
    finally: () => effects.push('finally'),
  });

  controller.close();
  controller.open({ id: 'sg-01' });
  resolveRequest(true);
  await request;

  assert.deepEqual(effects, []);
  assert.equal(dialog.open, true);
  assert.equal(controller.current().id, 'sg-01');
});

test('a stale dialog failure is ignored by the newly opened node', async () => {
  const dialog = dialogFixture();
  const controller = bindNodeContentDialog({ dialog, closeButton: new Control() });
  let rejectRequest;
  const effects = [];
  controller.open({ id: 'hk-02' });
  const request = runDialogSession(controller, () => new Promise((resolve, reject) => { rejectRequest = reject; }), {
    success: () => effects.push('success'),
    error: () => effects.push('error'),
    finally: () => effects.push('finally'),
  });

  controller.close();
  controller.open({ id: 'sg-01' });
  rejectRequest(new Error('旧请求失败'));
  await request;

  assert.deepEqual(effects, []);
  assert.equal(controller.current().id, 'sg-01');
});

test('closing and reopening the same node still invalidates the old submission', async () => {
  const dialog = dialogFixture();
  const controller = bindNodeContentDialog({ dialog, closeButton: new Control() });
  let resolveRequest;
  const effects = [];
  controller.open({ id: 'hk-02', revision: 7 });
  const request = runDialogSession(controller, () => new Promise(resolve => { resolveRequest = resolve; }), {
    success: () => effects.push('refresh-form'),
    finally: () => effects.push('enable-old-submit'),
  });

  controller.close();
  controller.open({ id: 'hk-02', revision: 8 });
  resolveRequest(false);
  await request;

  assert.deepEqual(effects, []);
  assert.equal(controller.current().revision, 8);
});
