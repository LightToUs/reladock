import test from 'node:test';
import assert from 'node:assert/strict';

import { workspaceSummary, ruleStatusPresentation } from '../public/workspace-summary.js';

test('workspace summary separates applied configuration from pending delivery and node contact', () => {
  const summary = workspaceSummary({
    rules: [
      { desiredVersion: 3, appliedVersion: 3, appliedStatus: 'active' },
      { desiredVersion: 4, appliedVersion: 3, appliedStatus: 'active' },
      { desiredVersion: 0, appliedVersion: 0, status: 'paused' },
    ],
    nodes: [
      { lastSeenAt: '2026-11-01T00:00:00Z' },
      { lastSeenAt: '' },
    ],
  });

  assert.deepEqual(summary, {
    totalRules: 3,
    appliedRules: 1,
    pendingRules: 2,
    contactedNodes: 1,
    totalNodes: 2,
  });
});

test('rule status presentation never describes applied configuration as healthy', () => {
  assert.deepEqual(
    ruleStatusPresentation({ desiredVersion: 2, appliedVersion: 2, appliedStatus: 'active' }),
    { label: '配置已应用', tone: 'applied', detail: '端到端健康待验证' },
  );
  assert.equal(
    ruleStatusPresentation({ desiredVersion: 2, appliedVersion: 1, appliedStatus: 'active' }).tone,
    'pending',
  );
  assert.equal(
    ruleStatusPresentation({ desiredVersion: 0, appliedVersion: 0, status: 'pending-agent' }).label,
    '待 Agent 部署',
  );
});
