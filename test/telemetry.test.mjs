import test from 'node:test';
import assert from 'node:assert/strict';

import { createLatestRequestGate, formatBytes, formatRate, loadWorkspaceData, telemetryInteractionState, telemetryPresentation, trendBars } from '../public/telemetry.js';

test('traffic formatting keeps cumulative bytes and display rates in distinct units', () => {
  assert.equal(formatBytes(0), '0 B');
  assert.equal(formatBytes(1536), '1.50 KiB');
  assert.equal(formatBytes(5 * 1024 ** 4), '5.00 TiB');
  assert.equal(formatRate(0), '0 bps');
  assert.equal(formatRate(125_000), '1.00 Mbps');
  assert.equal(formatRate(125_000_000), '1.00 Gbps');
});

test('fresh telemetry presents automatic totals, receive-time rate and uncapped utilization', () => {
  const view = telemetryPresentation({
    hasReports: true,
    uploadBytes: 1024,
    downloadBytes: 2048,
    totalBytes: 3072,
    rateState: 'fresh',
    uploadBytesPerSecond: 125_000,
    downloadBytesPerSecond: 250_000,
    totalBytesPerSecond: 375_000,
    utilizationBasisPoints: 12_500,
    rateWindowSeconds: 15,
    lastReportedAt: '2026-11-01T00:00:00Z',
  });

  assert.equal(view.total, '3.00 KiB');
  assert.equal(view.upload, '1.00 KiB');
  assert.equal(view.download, '2.00 KiB');
  assert.equal(view.rate, '3.00 Mbps');
  assert.equal(view.uploadRate, '1.00 Mbps');
  assert.equal(view.downloadRate, '2.00 Mbps');
  assert.equal(view.utilization, '125.00%');
  assert.equal(view.rateTone, 'fresh');
  assert.match(view.rateDetail, /15 秒/);
  assert.match(view.rateDetail, /平台接收时间/);
});

test('stale and insufficient rate states never imply zero traffic', () => {
  const stale = telemetryPresentation({ hasReports: true, uploadBytes: 1, downloadBytes: 2, totalBytes: 3, rateState: 'stale' });
  assert.equal(stale.rate, '近期速率过期');
  assert.equal(stale.utilization, '未知');
  assert.equal(stale.rateTone, 'stale');

  const insufficient = telemetryPresentation({ hasReports: true, uploadBytes: 1, downloadBytes: 2, totalBytes: 3, rateState: 'insufficient' });
  assert.equal(insufficient.rate, '近期样本不足');
  assert.equal(insufficient.utilization, '未知');
  assert.match(insufficient.rateDetail, /不推断为 0/);

  const empty = telemetryPresentation({ hasReports: false, uploadBytes: 0, downloadBytes: 0, totalBytes: 0, rateState: 'insufficient' });
  assert.equal(empty.total, '尚未收到流量报告');
});

test('sparse trend scaling retains only received buckets and handles an all-zero maximum', () => {
  const points = [
    { startedAt: '2026-11-01T00:00:00Z', totalBytes: 10, uploadBytes: 4, downloadBytes: 6 },
    { startedAt: '2026-11-01T00:15:00Z', totalBytes: 40, uploadBytes: 10, downloadBytes: 30 },
  ];
  const trend = trendBars(points, 60);
  assert.equal(trend.maxBytes, 40);
  assert.equal(trend.bars.length, 2);
  assert.deepEqual(trend.bars.map(point => point.ratio), [0.25, 1]);
  assert.deepEqual(trend.bars.map(point => point.slot), [0, 15]);
  assert.equal(trend.bars[1].startedAt, points[1].startedAt);

  const zeros = trendBars([{ startedAt: points[0].startedAt, totalBytes: 0, uploadBytes: 0, downloadBytes: 0 }]);
  assert.equal(zeros.maxBytes, 0);
  assert.equal(zeros.bars[0].ratio, 0);
});

test('latest-request gate rejects an older history response after selection changes', async () => {
  const gate = createLatestRequestGate();
  const rendered = [];
  let finishOld;
  const oldResponse = new Promise(resolve => { finishOld = resolve; });
  const oldToken = gate.next();
  const oldRender = oldResponse.then(value => { if (gate.isCurrent(oldToken)) rendered.push(value); });
  const newToken = gate.next();
  await Promise.resolve('rule-b').then(value => { if (gate.isCurrent(newToken)) rendered.push(value); });
  finishOld('rule-a');
  await oldRender;
  assert.deepEqual(rendered, ['rule-b']);
});

test('optional telemetry failures preserve the authenticated overview', async () => {
  const result = await loadWorkspaceData(async path => {
    if (path === '/api/overview') return { account: { email: 'owner@example.com' }, rules: [], nodes: [] };
    throw new Error(`${path} unavailable`);
  });
  assert.equal(result.overview.account.email, 'owner@example.com');
  assert.deepEqual(result.telemetry, []);
  assert.deepEqual(result.operationNodes, []);
  assert.equal(result.telemetryAvailable, false);
  assert.equal(result.resourcesAvailable, false);
  assert.equal(result.errors.length, 2);
});

test('an authentication failure from an optional endpoint is not downgraded to unavailable telemetry', async () => {
  const unauthorized = Object.assign(new Error('session expired'), { status: 401 });
  await assert.rejects(loadWorkspaceData(async path => {
    if (path === '/api/overview') return { account: {}, rules: [], nodes: [] };
    if (path === '/api/billing/telemetry') throw unauthorized;
    return { items: [] };
  }), error => error === unauthorized);
});

test('unavailable telemetry keeps history interaction disabled and explicitly unknown', () => {
  assert.deepEqual(telemetryInteractionState(false, 'rule-a'), { controlsDisabled: true, historyAllowed: false });
  assert.deepEqual(telemetryInteractionState(true, ''), { controlsDisabled: true, historyAllowed: false });
  assert.deepEqual(telemetryInteractionState(true, 'rule-a'), { controlsDisabled: false, historyAllowed: true });
});
