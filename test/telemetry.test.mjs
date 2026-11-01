import test from 'node:test';
import assert from 'node:assert/strict';

import { formatBytes, formatRate, telemetryPresentation, trendBars } from '../public/telemetry.js';

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
  const trend = trendBars(points);
  assert.equal(trend.maxBytes, 40);
  assert.equal(trend.bars.length, 2);
  assert.deepEqual(trend.bars.map(point => point.ratio), [0.25, 1]);
  assert.equal(trend.bars[1].startedAt, points[1].startedAt);

  const zeros = trendBars([{ startedAt: points[0].startedAt, totalBytes: 0, uploadBytes: 0, downloadBytes: 0 }]);
  assert.equal(zeros.maxBytes, 0);
  assert.equal(zeros.bars[0].ratio, 0);
});
