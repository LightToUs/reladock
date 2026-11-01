import test from 'node:test';
import assert from 'node:assert/strict';

import { createAdaptivePoller, loadAllOperationNodes, nodeMonitoringPresentation } from '../public/node-monitoring.js';

test('node monitoring presents fresh traffic, resources and Agent runtime independently', () => {
  const now = new Date('2026-11-01T12:00:00Z');
  const view = nodeMonitoringPresentation({
    contactStatus: 'online',
    dataPlaneState: 'in-sync',
    hasReports: true,
    rateState: 'fresh',
    uploadBytes: 1024,
    downloadBytes: 2048,
    totalBytes: 3072,
    uploadBytesPerSecond: 125_000,
    downloadBytesPerSecond: 250_000,
    totalBytesPerSecond: 375_000,
    lastReportedAt: '2026-11-01T11:59:55Z',
    resourceStatus: 'normal',
    resourceObservedAt: '2026-11-01T11:59:30Z',
    cpuPercent: 7,
    memoryPercent: 18,
    diskPercent: 14,
    conntrackPercent: 3,
    agentStartedAt: '2026-10-30T12:00:00Z',
  }, now);

  assert.equal(view.contact.label, '在线');
  assert.equal(view.dataPlane.state, 'in-sync');
  assert.equal(view.traffic.uploadRate, '1.00 Mbps');
  assert.equal(view.traffic.downloadRate, '2.00 Mbps');
  assert.equal(view.traffic.uploadTotal, '1.00 KiB');
  assert.equal(view.traffic.downloadTotal, '2.00 KiB');
  assert.equal(view.resources.state, 'fresh');
  assert.deepEqual(view.resources.cpu, { value: 7, label: '7%' });
  assert.equal(view.agent.runtime, '2 天');
  assert.match(view.agent.detail, /Agent/);
  assert.doesNotMatch(view.agent.runtime, /VPS/);
});

test('node monitoring keeps no-report, insufficient, stale and unknown values explicit', () => {
  const now = new Date('2026-11-01T12:00:00Z');
  const none = nodeMonitoringPresentation({ rateState: 'none', hasReports: false }, now);
  assert.equal(none.traffic.uploadRate, '尚无报告');
  assert.equal(none.traffic.uploadTotal, '尚无报告');
  assert.equal(none.resources.cpu.label, '未知');
  assert.equal(none.agent.runtime, '未知');

  const insufficient = nodeMonitoringPresentation({ rateState: 'insufficient', hasReports: true, uploadBytes: 4, downloadBytes: 6, totalBytes: 10 }, now);
  assert.equal(insufficient.traffic.uploadRate, '样本不足');
  assert.equal(insufficient.traffic.total, '10 B');

  const stale = nodeMonitoringPresentation({
    rateState: 'stale', hasReports: true, uploadBytes: 4, downloadBytes: 6, totalBytes: 10,
    resourceObservedAt: '2026-11-01T11:58:29Z', cpuPercent: 20,
  }, now);
  assert.equal(stale.traffic.uploadRate, '速率已过期');
  assert.equal(stale.traffic.uploadTotal, '4 B');
  assert.equal(stale.resources.state, 'stale');
  assert.equal(stale.resources.cpu.label, '数据已过期');

  const serverStale = nodeMonitoringPresentation({
    resourceStatus: 'stale', resourceObservedAt: '2026-11-01T11:59:59Z', cpuPercent: 20,
  }, now);
  assert.equal(serverStale.resources.state, 'stale');
  assert.equal(serverStale.resources.cpu.label, '数据已过期');
});

test('offline Agent runtime stops at the last heartbeat', () => {
  const view = nodeMonitoringPresentation({
    contactStatus: 'offline',
    agentStartedAt: '2026-11-01T09:00:00Z',
    agentHeartbeatAt: '2026-11-01T10:30:00Z',
  }, new Date('2026-11-01T12:00:00Z'));
  assert.equal(view.contact.label, '离线');
  assert.equal(view.agent.runtime, '1 小时');
  assert.equal(view.agent.label, '最后上报运行时长');
});

test('operation node loader retrieves every bounded API page', async () => {
  const calls = [];
  const nodes = await loadAllOperationNodes(async path => {
    calls.push(path);
    const page = Number(new URL(path, 'https://console.example').searchParams.get('page'));
    return { items: [{ id: `node-${page}` }], page, pageSize: 100, total: 201 };
  }, '/api/operations/nodes?staleAfterSeconds=90');
  assert.deepEqual(nodes.map(node => node.id), ['node-1', 'node-2', 'node-3']);
  assert.deepEqual(calls.map(path => new URL(path, 'https://console.example').searchParams.get('page')), ['1', '2', '3']);
});

function fakeScheduler() {
  let nextID = 1;
  const timers = new Map();
  return {
    timers,
    setTimer(fn, ms) {
      const id = nextID++;
      timers.set(id, { fn, ms });
      return id;
    },
    clearTimer(id) { timers.delete(id); },
    async fire(ms) {
      const entry = [...timers.entries()].find(([, timer]) => timer.ms === ms);
      assert.ok(entry, `missing ${ms}ms timer`);
      timers.delete(entry[0]);
      entry[1].fn();
      await Promise.resolve();
      await Promise.resolve();
    },
  };
}

function fakeDocument() {
  const listeners = new Set();
  return {
    hidden: false,
    addEventListener(type, listener) { if (type === 'visibilitychange') listeners.add(listener); },
    removeEventListener(type, listener) { if (type === 'visibilitychange') listeners.delete(listener); },
    setHidden(value) {
      this.hidden = value;
      for (const listener of listeners) listener();
    },
  };
}

test('adaptive poller uses visible and hidden cadence with immediate visible refresh', async () => {
  const scheduler = fakeScheduler();
  const documentTarget = fakeDocument();
  let runs = 0;
  const poller = createAdaptivePoller({ run: async () => { runs += 1; }, documentTarget, activeMs: 5000, hiddenMs: 30000, ...scheduler });
  await poller.start();
  assert.equal(runs, 1);
  assert.equal([...scheduler.timers.values()][0].ms, 5000);
  documentTarget.setHidden(true);
  assert.equal([...scheduler.timers.values()][0].ms, 30000);
  await scheduler.fire(30000);
  assert.equal(runs, 2);
  documentTarget.setHidden(false);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(runs, 3);
  assert.equal([...scheduler.timers.values()][0].ms, 5000);
  poller.stop();
  assert.equal(scheduler.timers.size, 0);
});

test('adaptive poller never overlaps, recovers after rejection and stops cleanly', async () => {
  const scheduler = fakeScheduler();
  const documentTarget = fakeDocument();
  let resolveFirst;
  let active = 0;
  let maximum = 0;
  let runs = 0;
  const poller = createAdaptivePoller({
    documentTarget,
    activeMs: 5000,
    hiddenMs: 30000,
    ...scheduler,
    run: () => {
      runs += 1;
      active += 1;
      maximum = Math.max(maximum, active);
      if (runs === 1) return new Promise(resolve => { resolveFirst = () => { active -= 1; resolve(); }; });
      active -= 1;
      if (runs === 2) return Promise.reject(new Error('temporary'));
      return Promise.resolve();
    },
  });
  const starting = poller.start();
  const duplicate = poller.refresh();
  assert.equal(runs, 1);
  assert.equal(runs, 1);
  resolveFirst();
  await Promise.all([starting, duplicate]);
  assert.equal(maximum, 1);
  await scheduler.fire(5000);
  assert.equal(runs, 2);
  assert.equal([...scheduler.timers.values()][0].ms, 5000);
  await scheduler.fire(5000);
  assert.equal(runs, 3);
  poller.stop();
  await poller.refresh();
  assert.equal(runs, 3);
});

test('stopping an unresolved poll invalidates its commit before a new session starts', async () => {
  const scheduler = fakeScheduler();
  const documentTarget = fakeDocument();
  const commits = [];
  let resolveOld;
  let oldSignal;
  let runs = 0;
  const poller = createAdaptivePoller({
    documentTarget,
    ...scheduler,
    run: async cycle => {
      runs += 1;
      if (runs === 1) oldSignal = cycle.signal;
      const value = runs === 1 ? await new Promise(resolve => { resolveOld = resolve; }) : 'new';
      if (cycle.isCurrent()) commits.push(value);
    },
  });
  const old = poller.start();
  poller.stop();
  assert.equal(oldSignal.aborted, true);
  const restarted = poller.start();
  assert.equal(runs, 1);
  resolveOld('old');
  await Promise.all([old, restarted]);
  assert.deepEqual(commits, ['new']);
});
