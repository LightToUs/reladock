import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { sourceBlockCIDR, sourceBlockPresentation, sourceBlockRequestForFlow, runCommittedSourceBlockMutation } from './source-ip-blocks.js';

test('source flow action builds a rule-scoped IPv4 /32 request', () => {
  const flow = { tenantId: 'tenant-a', ruleId: 'rule-a', sourceIp: '198.51.100.23', eventId: 'event-a' };
  assert.deepEqual(sourceBlockRequestForFlow(flow, 7), {
    path: '/api/admin/rules/rule-a/source-blocks',
    body: { tenantId: 'tenant-a', desiredVersion: 7, cidr: '198.51.100.23/32', reason: '来源连接记录 event-a' },
  });
  assert.equal(sourceBlockCIDR('198.51.100.23'), '198.51.100.23/32');
  assert.throws(() => sourceBlockCIDR('2001:db8::1'));
  assert.throws(() => sourceBlockCIDR('999.1.1.1'));
});

test('block presentation distinguishes requested from applied state', () => {
  assert.deepEqual(sourceBlockPresentation({ cidr: '198.51.100.0/24', requestedVersion: 4, applied: false }), {
    label: '等待节点应用', detail: '请求版本 4；尚未生效',
  });
  assert.deepEqual(sourceBlockPresentation({ cidr: '198.51.100.0/24', requestedVersion: 4, applied: true }), {
    label: '节点已应用', detail: '应用确认不代表端到端健康',
  });
  assert.deepEqual(sourceBlockPresentation({ cidr: '198.51.100.0/24', state: 'removed', removedVersion: 5, applied: false }), {
    label: '等待节点移除', detail: '移除版本 5；节点仍可能执行此拉黑',
  });
});

test('committed mutation never resubmits when the refresh fails', async () => {
  let writes = 0;
  const error = await runCommittedSourceBlockMutation({
    mutate: async () => { writes += 1; return { id: 'block-a' }; },
    refresh: async () => { throw new Error('refresh failed'); },
  }).catch(value => value);
  assert.equal(writes, 1);
  assert.equal(error.committed, true);
  assert.match(error.message, /refresh failed/);
});

test('admin page exposes manual management and source-flow one-click controls', () => {
  const html = readFileSync(new URL('./admin.html', import.meta.url), 'utf8');
  const js = readFileSync(new URL('./admin.js', import.meta.url), 'utf8');
  for (const value of ['source-block-dialog', 'source-block-form', 'IPv4 地址或 CIDR', '等待对应节点应用确认']) assert.match(html, new RegExp(value));
  for (const value of ['拉黑此 IP', '来源拉黑', 'sourceBlockRequestForFlow', '规则已被其他操作更新', '拉黑已提交，但状态刷新失败']) assert.match(js, new RegExp(value));
});

