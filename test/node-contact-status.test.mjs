import test from 'node:test';
import assert from 'node:assert/strict';
import { agentContactPresentation } from '../public/node-contact-status.js';

test('agent contact status has distinct operator-facing states', () => {
  assert.deepEqual(agentContactPresentation('online'), { label: '在线', tone: 'online' });
  assert.deepEqual(agentContactPresentation('offline'), { label: '离线', tone: 'offline' });
  assert.deepEqual(agentContactPresentation('never-seen'), { label: '待 Agent 连接', tone: 'pending' });
  assert.deepEqual(agentContactPresentation('unexpected'), { label: '状态未知', tone: 'unknown' });
});
