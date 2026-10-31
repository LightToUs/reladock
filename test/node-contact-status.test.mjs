import test from 'node:test';
import assert from 'node:assert/strict';
import { agentContactPresentation, certificatePresentation, configurationPresentation, probePresentation } from '../public/node-contact-status.js';

test('agent contact status has distinct operator-facing states', () => {
  assert.deepEqual(agentContactPresentation('online'), { label: '在线', tone: 'online', detail: '最近 90 秒收到心跳；不代表转发健康' });
  assert.deepEqual(agentContactPresentation('offline'), { label: '离线', tone: 'offline', detail: '心跳已超时；Agent 会自动重连，尚未确认恢复' });
  assert.deepEqual(agentContactPresentation('never-seen'), { label: '待 Agent 连接', tone: 'pending', detail: '尚无 Agent 心跳记录' });
  assert.deepEqual(agentContactPresentation('unexpected'), { label: '状态未知', tone: 'unknown', detail: '没有可确认的 Agent 联系状态' });
});

test('certificate status distinguishes expiry and bounded rotation states', () => {
  const now = new Date('2026-10-31T00:00:00Z');
  assert.deepEqual(certificatePresentation({}, now), { label: '未绑定', tone: 'unknown', detail: '没有节点证书' });
  assert.equal(certificatePresentation({ certificateFingerprint: 'a', certificateNotAfter: '2026-11-30T00:00:00Z', rotationState: 'idle' }, now).label, '正常');
  assert.equal(certificatePresentation({ certificateFingerprint: 'a', certificateNotAfter: '2026-11-06T00:00:00Z', rotationState: 'idle' }, now).label, '即将到期');
  assert.equal(certificatePresentation({ certificateFingerprint: 'a', certificateNotAfter: '2026-10-30T00:00:00Z', rotationState: 'idle' }, now).label, '已过期');
  assert.equal(certificatePresentation({ certificateFingerprint: 'a', certificateNotAfter: '2026-11-30T00:00:00Z', rotationState: 'pending' }, now).label, '轮换中');
  const failed = certificatePresentation({ certificateFingerprint: 'a', certificateNotAfter: '2026-11-30T00:00:00Z', rotationState: 'error', rotationErrorCode: 'renew-unavailable' }, now);
  assert.equal(failed.label, '轮换失败');
  assert.match(failed.detail, /renew-unavailable/);
  assert.equal(certificatePresentation({ certificateFingerprint: 'a', certificateNotAfter: '2026-10-30T00:00:00Z', rotationState: 'error', rotationErrorCode: 'renew-unavailable' }, now).label, '已过期 · 轮换失败');
});

test('configuration status never turns acknowledgement into health', () => {
  assert.equal(configurationPresentation({ desiredGeneration: 3, validatedGeneration: 3, appliedGeneration: 3 }).label, '已应用 · 健康未知');
  assert.equal(configurationPresentation({ desiredGeneration: 3, validatedGeneration: 3, appliedGeneration: 0 }).label, '校验通过 · 未应用');
  assert.match(configurationPresentation({ desiredGeneration: 3, validatedGeneration: 2, appliedGeneration: 2 }).detail, /端到端健康未知/);
});

test('probe status is reachability metadata, never end-to-end health', () => {
  assert.equal(probePresentation({ probeStatus: 'targetReachable' }).label, '目标 TCP 可达');
  assert.match(probePresentation({ probeStatus: 'targetReachable' }).detail, /不代表客户端端到端健康/);
  assert.equal(probePresentation({ probeStatus: 'targetUnreachable' }).label, '目标 TCP 不可达');
  assert.equal(probePresentation({ probeStatus: 'udpUnknown' }).label, 'UDP 健康未知');
  assert.equal(probePresentation({ probeStatus: 'unknown' }).label, '未有当前探测');
});
