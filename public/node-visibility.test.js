import assert from 'node:assert/strict';
import test from 'node:test';

import { bindNodeVisibilityToggle, nodesForAdminList } from './node-visibility.js';

test('the default admin list hides archived nodes without removing them from shared state', () => {
  const nodes = [
    { id: 'hk-01', hidden: true },
    { id: 'sg-01', hidden: false },
  ];

  assert.deepEqual(nodesForAdminList(nodes, false).map(node => node.id), ['sg-01']);
  assert.deepEqual(nodesForAdminList(nodes, true).map(node => node.id), ['hk-01', 'sg-01']);
  assert.equal(nodes.length, 2);
});

test('the show-hidden checkbox drives list rendering in both directions', () => {
  const checkbox = {
    checked: false,
    addEventListener(type, handler) { assert.equal(type, 'change'); this.change = handler; },
  };
  const rendered = [];
  bindNodeVisibilityToggle({ checkbox, onChange: showHidden => rendered.push(showHidden) });

  checkbox.checked = true;
  checkbox.change();
  checkbox.checked = false;
  checkbox.change();

  assert.deepEqual(rendered, [true, false]);
});
