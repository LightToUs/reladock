import assert from 'node:assert/strict';
import test from 'node:test';

import { nodeWithdrawalControl } from './node-withdrawal.js';

test('withdrawal control is absent for nodes with lifecycle history', () => {
  assert.equal(nodeWithdrawalControl({ id: 'used-01', revision: 4, withdrawable: false }), null);
});

test('withdrawal control describes the guarded undeployed-node action', () => {
  assert.deepEqual(nodeWithdrawalControl({ id: 'unused-01', name: '误登记节点', revision: 3, withdrawable: true }), {
    label: '撤销登记',
    path: '/api/admin/nodes/unused-01/withdraw-registration',
    body: { revision: 3 },
    confirmText: '撤销“误登记节点”的节点登记？仅从未部署且没有任何业务或运行历史的空节点可以撤销；此操作不可恢复。',
    successText: '未部署的空节点登记已撤销。',
    danger: true,
  });
});
