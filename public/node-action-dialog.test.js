import assert from 'node:assert/strict';
import test from 'node:test';

import { bindNodeActionDialog, createNodeActionRunner, nodeActionDescriptor } from './node-action-dialog.js';

test('certificate rotation requires capable enrolled agent and reports a request, not completion', () => {
  const node = {id: 'hk-01', name: '香港', revision: 8, certificateFingerprint: 'abc', certificateRotationSupported: true};
  const action = nodeActionDescriptor('rotate-certificate', node);
  assert.equal(action.path, '/api/admin/nodes/hk-01/rotate-certificate');
  assert.deepEqual(action.body, {revision: 8});
  assert.match(action.successText, /等待/);
  assert.equal(nodeActionDescriptor('rotate-certificate', {...node, certificateRotationSupported: false}), null);
  assert.equal(nodeActionDescriptor('rotate-certificate', {...node, certificateFingerprint: ''}), null);
  assert.equal(nodeActionDescriptor('rotate-certificate', {...node, certificateRotationRequested: true}), null);
});

class Control extends EventTarget {
  constructor() {
    super();
    this.textContent = '';
    this.disabled = false;
    this.classes = new Set();
    this.classList = { toggle: (name, force) => force ? this.classes.add(name) : this.classes.delete(name) };
  }
  click() { this.dispatchEvent(new Event('click')); }
}

function fixture() {
  const dialog = new EventTarget();
  dialog.open = false;
  dialog.showModal = () => { dialog.open = true; };
  dialog.close = () => { dialog.open = false; dialog.dispatchEvent(new Event('close')); };
  const ui = {
    dialog,
    title: { textContent: '' },
    description: { textContent: '' },
    status: { textContent: '' },
    confirmButton: new Control(),
    cancelButton: new Control(),
  };
  return { ui, controller: bindNodeActionDialog(ui) };
}

const flush = () => new Promise(resolve => setImmediate(resolve));

test('opening an action shows its content and cancellation never runs it', () => {
  const { ui, controller } = fixture();
  let calls = 0;
  controller.open({ title: '进入维护', description: '停止新增分配。', confirmLabel: '确认进入维护', danger: false, run: async () => { calls++; } });

  assert.equal(ui.dialog.open, true);
  assert.equal(ui.title.textContent, '进入维护');
  assert.equal(ui.description.textContent, '停止新增分配。');
  assert.equal(ui.confirmButton.textContent, '确认进入维护');
  assert.equal(ui.confirmButton.classes.has('danger'), false);

  ui.cancelButton.click();
  assert.equal(ui.dialog.open, false);
  assert.equal(calls, 0);
});

test('confirmation runs once and keeps an actionable error in the dialog', async () => {
  const { ui, controller } = fixture();
  let reject;
  let calls = 0;
  controller.open({ title: '重试交付', description: '生成新快照。', confirmLabel: '确认重试', danger: true, run: () => { calls++; return new Promise((_, fail) => { reject = fail; }); } });

  ui.confirmButton.click();
  ui.confirmButton.click();
  assert.equal(calls, 1);
  assert.equal(ui.confirmButton.disabled, true);
  assert.equal(ui.cancelButton.disabled, true);

  reject(new Error('修订号已变化'));
  await flush();
  assert.equal(ui.dialog.open, true);
  assert.equal(ui.status.textContent, '修订号已变化');
  assert.equal(ui.confirmButton.disabled, false);
  assert.equal(ui.cancelButton.disabled, false);
});

test('successful confirmation closes the dialog', async () => {
  const { ui, controller } = fixture();
  controller.open({ title: '恢复分配', description: '允许新增分配。', confirmLabel: '确认恢复', danger: false, run: async () => {} });

  ui.confirmButton.click();
  await flush();

  assert.equal(ui.dialog.open, false);
  assert.equal(ui.status.textContent, '');
});

test('every node-row state change has a dialog action descriptor', () => {
  const node = { id: 'hk-02', name: '香港入口 02', revision: 7, visibilityRevision: 3, withdrawable: true };
  const expected = {
    maintenance: ['/api/admin/nodes/hk-02/maintenance', '进入维护', { revision: 7 }],
    enable: ['/api/admin/nodes/hk-02/enable', '恢复分配', { revision: 7 }],
    retry: ['/api/admin/nodes/hk-02/retry', '重试配置交付', { revision: 7 }],
    upgrade: ['/api/admin/nodes/hk-02/upgrade', '升级 Agent', { revision: 7 }],
    'upgrade-cancel': ['/api/admin/nodes/hk-02/upgrade-cancel', '取消 Agent 升级', { revision: 7 }],
    'start-draining': ['/api/admin/nodes/hk-02/start-draining', '进入安全排空', { revision: 7 }],
    'finalize-retirement': ['/api/admin/nodes/hk-02/finalize-retirement', '最终退役节点', { revision: 7 }],
    'withdraw-registration': ['/api/admin/nodes/hk-02/withdraw-registration', '撤销节点登记', { revision: 7 }],
    'revoke-certificate': ['/api/admin/nodes/hk-02/revoke-certificate', '撤销节点证书', { revision: 7 }],
    hide: ['/api/admin/nodes/hk-02/visibility', '隐藏节点', { visibilityRevision: 3, hidden: true }],
    unhide: ['/api/admin/nodes/hk-02/visibility', '恢复显示', { visibilityRevision: 3, hidden: false }],
  };

  for (const [kind, [path, title, body]] of Object.entries(expected)) {
    const action = nodeActionDescriptor(kind, node);
    assert.equal(action.path, path, kind);
    assert.equal(action.title, `${title} · 香港入口 02`, kind);
    assert.deepEqual(action.body, body, kind);
    assert.ok(action.description.length > 12, kind);
    assert.ok(action.confirmLabel.length > 1, kind);
  }
  assert.equal(nodeActionDescriptor('withdraw-registration', { ...node, withdrawable: false }), null);
  assert.equal(nodeActionDescriptor('unknown', node), null);
});

test('retrying after a revision conflict uses the freshly loaded node revision', async () => {
  const requests = [];
  let current = { id: 'hk-02', name: '香港入口 02', revision: 7 };
  const run = createNodeActionRunner({
    kind: 'maintenance',
    node: current,
    getNode: () => current,
    mutate: async (path, body) => {
      requests.push([path, body]);
      if (requests.length === 1) { current = { ...current, revision: 8 }; return false; }
      return true;
    },
    conflictMessage: () => '操作冲突，已载入最新数据。',
  });

  await assert.rejects(run, /操作冲突/);
  await run();

  assert.deepEqual(requests, [
    ['/api/admin/nodes/hk-02/maintenance', { revision: 7 }],
    ['/api/admin/nodes/hk-02/maintenance', { revision: 8 }],
  ]);
});

test('a background refresh cannot silently change the first confirmed revision', async () => {
  const requests = [];
  const refreshedActions = [];
  const shown = { id: 'hk-02', name: '香港入口 02', revision: 7 };
  let current = { ...shown, name: '香港入口新名称', revision: 8 };
  const run = createNodeActionRunner({
    kind: 'maintenance',
    node: shown,
    getNode: () => current,
    mutate: async (path, body) => { requests.push([path, body]); return requests.length > 1; },
    conflictMessage: () => '操作冲突，已载入最新数据。',
    onRefresh: action => refreshedActions.push(action),
  });

  await assert.rejects(run, /操作冲突/);
  assert.deepEqual(requests, [['/api/admin/nodes/hk-02/maintenance', { revision: 7 }]]);
  assert.equal(refreshedActions[0].title, '进入维护 · 香港入口新名称');

  await run();
  assert.deepEqual(requests[1], ['/api/admin/nodes/hk-02/maintenance', { revision: 8 }]);
});

test('updating an open confirmation replaces displayed details but keeps its run callback', async () => {
  const { ui, controller } = fixture();
  let calls = 0;
  controller.open({ title: '进入维护 · 旧名称', description: '旧说明', confirmLabel: '旧确认', danger: false, run: async () => { calls++; } });

  controller.update({ title: '进入维护 · 新名称', description: '请重新检查最新状态', confirmLabel: '重新确认', danger: true });
  assert.equal(ui.title.textContent, '进入维护 · 新名称');
  assert.equal(ui.description.textContent, '请重新检查最新状态');
  assert.equal(ui.confirmButton.textContent, '重新确认');
  assert.equal(ui.confirmButton.classes.has('danger'), true);

  ui.confirmButton.click();
  await flush();
  assert.equal(calls, 1);
});
