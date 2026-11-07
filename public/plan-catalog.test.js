import assert from 'node:assert/strict';
import test from 'node:test';
import { adminPlanOfferingLabel, createOfferingMutationRunner, offeringActionDescriptor, offeringRouteLabel, offeringStateLabel, planCatalogLabel, planInputFromFields, priceableOfferings } from './plan-catalog.js';

const offering = { id: 'offer-hk', name: '香港优化线路', nodeId: 'node-01', sourceMarket: '中国大陆', destinationMarket: '香港 / 亚洲', state: 'published', resourceMode: 'shared' };

test('plan creation converts USD to integer cents and leaves version assignment to the server', () => {
  assert.deepEqual(planInputFromFields({ offeringId: 'offer-hk', code: '001', name: '标准套餐', priceUSD: '100.25', quotaGB: '1000', bandwidthMbps: '100', periodDays: '30', resourceMode: 'dedicated-rule' }), {
    offeringId: 'offer-hk', code: '001', name: '标准套餐', priceCents: 10025, quotaBytes: 1e12, bandwidthMbps: 100, periodSeconds: 2592000,
  });
});

test('USD conversion preserves cent boundaries across the accepted range', () => {
  for (const [priceUSD, priceCents] of [['0.01', 1], ['0.29', 29], ['999999.99', 99999999]]) {
    const input = planInputFromFields({ offeringId: 'offer-hk', code: '001', name: '标准套餐', priceUSD, quotaGB: '1', bandwidthMbps: '1', periodDays: '1' });
    assert.equal(input.priceCents, priceCents);
  }
});

test('retired offerings are unavailable for new price versions', () => {
  const choices = priceableOfferings([offering, { ...offering, id: 'draft', state: 'draft' }, { ...offering, id: 'retired', state: 'retired' }]);
  assert.deepEqual(choices.map(item => item.id), ['offer-hk', 'draft']);
});

test('customer plan labels include route identity and independent limits', () => {
  const label = planCatalogLabel({ name: '标准套餐', offering, resourceMode: 'shared', quotaBytes: 1e12, bandwidthMbps: 100, periodSeconds: 2592000, priceCents: 1999 });
  assert.match(label, /香港优化线路 · 中国大陆 → 香港 \/ 亚洲/);
  assert.match(label, /共享资源/);
  assert.match(label, /1,000 GB/);
  assert.match(label, /100 Mbps/);
  assert.match(label, /30 天/);
  assert.match(label, /\$19\.99/);
});

test('admin labels keep legacy unbound plans explicit and translate sale states', () => {
  assert.equal(adminPlanOfferingLabel({ offering: null }), '历史版本 · 未绑定线路 · 不可新购');
  assert.equal(adminPlanOfferingLabel({ offering }), '香港优化线路 · node-01 · 中国大陆 → 香港 / 亚洲');
  assert.equal(offeringStateLabel('sold-out'), '售罄');
  assert.equal(offeringStateLabel('retired'), '已下架');
});

test('offering route labels make optional markets explicit', () => {
  assert.equal(offeringRouteLabel({ sourceMarket: '', destinationMarket: '' }), '来源未填写 → 目标未填写');
  assert.equal(offeringRouteLabel(offering), '中国大陆 → 香港 / 亚洲');
});

test('every offering management operation has a dialog descriptor', () => {
  const draft = { ...offering, state: 'draft', revision: 4, deletable: true };
  assert.deepEqual(offeringActionDescriptor('delete', draft), {
    title: '删除草稿线路 · 香港优化线路',
    description: '此草稿尚未关联任何套餐价格版本。删除后不可恢复，平台审计仍会保留。',
    confirmLabel: '确认删除草稿',
    danger: true,
    path: '/api/admin/offerings/delete',
    body: { offeringId: 'offer-hk', expectedRevision: 4 },
    successText: '草稿线路已删除。',
  });
  const publish = offeringActionDescriptor('published', draft);
  assert.equal(publish.path, '/api/admin/offerings/state');
  assert.deepEqual(publish.body, { offeringId: 'offer-hk', expectedRevision: 4, state: 'published' });
  assert.match(publish.description, /入口节点/);
  assert.match(publish.description, /未部署.*也可发布/);
  assert.equal(offeringActionDescriptor('delete', { ...draft, deletable: false }), null);
  assert.equal(offeringActionDescriptor('delete', { ...draft, state: 'paused' }), null);
  assert.equal(offeringActionDescriptor('unknown', draft), null);
});

test('a committed offering mutation retries only the failed list refresh', async () => {
  let mutations = 0;
  let refreshes = 0;
  let refreshFailed;
  const run = createOfferingMutationRunner({
    mutate: async () => { mutations++; },
    refresh: async () => { refreshes++; if (refreshes === 1) throw new Error('network unavailable'); },
    committedText: '草稿线路已删除',
    onRefreshFailure: error => { refreshFailed = error.message; },
  });

  await assert.rejects(run, /草稿线路已删除.*列表刷新失败.*再次确认只会重试刷新/);
  assert.equal(run.committed(), true);
  assert.equal(refreshFailed, 'network unavailable');
  await run();
  assert.equal(mutations, 1);
  assert.equal(refreshes, 2);
});
