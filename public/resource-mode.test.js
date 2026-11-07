import assert from 'node:assert/strict';
import test from 'node:test';

import { planMatchesNode, resourceModeLabel } from './resource-mode.js';

test('resource modes have explicit Chinese labels', () => {
  assert.equal(resourceModeLabel('shared'), '共享资源');
  assert.equal(resourceModeLabel('dedicated-tenant'), '企业专属资源');
  assert.equal(resourceModeLabel('dedicated-rule'), '规则专属资源');
  assert.equal(resourceModeLabel('future-mode'), '未知资源模式（future-mode）');
  assert.equal(resourceModeLabel(''), '未知资源模式（空）');
});

test('plan compatibility requires an exact resource mode match', () => {
  assert.equal(planMatchesNode({ resourceMode: 'shared' }, { resourceMode: 'shared' }), true);
  assert.equal(planMatchesNode({ resourceMode: 'shared' }, { resourceMode: 'dedicated-tenant' }), false);
  assert.equal(planMatchesNode({ resourceMode: '' }, { resourceMode: '' }), false);
  assert.equal(planMatchesNode({}, {}), false);
});
