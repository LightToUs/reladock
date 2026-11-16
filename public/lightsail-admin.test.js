import assert from 'node:assert/strict';
import test from 'node:test';

import {
  LIGHTSAIL_SCRIPT_WARNING,
  actionRequest,
  createActivationGate,
  createCatalogLoader,
  createLightsailPayload,
  createLightsailSubmission,
  instancePresentation,
  pollingDelay,
  safeInstanceView,
  setSafeText,
  statusPresentation,
} from './lightsail-admin.js';

const fields = {
  name: 'Singapore worker', region: 'ap-southeast-1', availabilityZone: 'ap-southeast-1a',
  blueprintId: 'debian_12', bundleId: 'small_3_0', script: '#!/bin/bash\necho once\n',
};

test('create payload is deterministic and preserves the pasted script exactly once', () => {
  const payload = createLightsailPayload(fields);
  assert.deepEqual(payload, {
    name: 'Singapore worker',
    selection: { region: 'ap-southeast-1', availabilityZone: 'ap-southeast-1a', blueprintId: 'debian_12', bundleId: 'small_3_0' },
    script: '#!/bin/bash\necho once\n',
  });
  assert.match(LIGHTSAIL_SCRIPT_WARNING, /只执行一次/);
  assert.match(LIGHTSAIL_SCRIPT_WARNING, /unknown|未知/);
});

test('create payload preserves an intentionally empty script', () => {
  assert.equal(createLightsailPayload({ ...fields, script: '' }).script, '');
  assert.deepEqual(instancePresentation({ lifecycleState: 'running', deploymentState: 'not-requested' }), {
    lifecycle: '实例运行中', deployment: '未要求执行脚本', detail: '不代表 RelaDock 转发或端到端健康', actions: ['stop'],
  });
});

test('uncertain create retry retains the same idempotency key', async () => {
  const calls = [];
  let attempt = 0;
  const submit = createLightsailSubmission({
    keyFactory: () => 'fixed-create-key',
    post: async (payload, key) => {
      calls.push({ payload, key });
      attempt += 1;
      if (attempt === 1) throw new Error('response lost');
      return { id: 'instance-1' };
    },
  });
  await assert.rejects(() => submit(fields), /response lost/);
  await submit(fields);
  assert.deepEqual(calls.map(call => call.key), ['fixed-create-key', 'fixed-create-key']);
  assert.deepEqual(calls[0].payload, calls[1].payload);
});

test('catalog loader ignores a stale response after region changes', async () => {
  const pending = new Map();
  const applied = [];
  const loader = createCatalogLoader({
    fetchCatalog: region => new Promise(resolve => pending.set(region, resolve)),
    apply: catalog => applied.push(catalog.region),
  });
  const first = loader.load('us-east-1');
  const second = loader.load('eu-west-1');
  pending.get('eu-west-1')({ region: 'eu-west-1' });
  await second;
  pending.get('us-east-1')({ region: 'us-east-1' });
  await first;
  assert.deepEqual(applied, ['eu-west-1']);
});

test('stopping invalidates an unresolved page activation', () => {
  const gate = createActivationGate();
  const first = gate.begin();
  assert.equal(first.isCurrent(), true);
  gate.stop();
  assert.equal(first.isCurrent(), false);
  const second = gate.begin();
  assert.equal(second.isCurrent(), true);
  assert.equal(first.isCurrent(), false);
});

test('disabled and lifecycle presentations never claim relay health', () => {
  assert.deepEqual(statusPresentation({ enabled: false }), { label: '未启用', detail: '未配置 AWS Lightsail 连接器。' });
  assert.deepEqual(instancePresentation({ lifecycleState: 'running', deploymentState: 'succeeded' }), {
    lifecycle: '实例运行中', deployment: '脚本执行成功', detail: '不代表 RelaDock 转发或端到端健康', actions: ['stop'],
  });
  assert.deepEqual(instancePresentation({ lifecycleState: 'stopped', deploymentState: 'unknown' }).actions, ['destroy']);
});

test('action payloads are revision guarded and destroy repeats the exact name', () => {
  const instance = { id: 'id-1', revision: 7, awsInstanceName: 'reladock-ls-abc' };
  assert.deepEqual(actionRequest(instance, 'stop'), { path: '/api/admin/lightsail/instances/id-1/stop', body: { revision: 7 } });
  assert.deepEqual(actionRequest(instance, 'destroy', 'reladock-ls-abc'), { path: '/api/admin/lightsail/instances/id-1/destroy', body: { revision: 7, confirmation: 'reladock-ls-abc' } });
  assert.throws(() => actionRequest(instance, 'destroy', 'wrong'), /实例名称/);
});

test('safe view and text rendering exclude scripts and credentials', () => {
  const source = { id: 'one', name: '<img src=x>', script: 'secret script', privateKey: 'private', certificate: 'cert', executionOutput: 'ok' };
  assert.deepEqual(safeInstanceView(source), { id: 'one', name: '<img src=x>', executionOutput: 'ok' });
  const node = { textContent: '', set innerHTML(_) { throw new Error('unsafe HTML'); } };
  setSafeText(node, source.name);
  assert.equal(node.textContent, '<img src=x>');
  assert.equal(pollingDelay(false), 5000);
  assert.equal(pollingDelay(true), 30000);
});
