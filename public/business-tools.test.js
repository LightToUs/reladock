import assert from 'node:assert/strict';
import test from 'node:test';
import * as business from './business-tools.js';

const plan = {
  id: 'plan-hk-1000',
  name: '标准套餐',
  priceCents: 1999,
  quotaBytes: 1e12,
  bandwidthMbps: 100,
  periodSeconds: 2592000,
  resourceMode: 'shared',
  offering: {
    id: 'offering-hk',
    code: 'hk-mainland',
    name: '香港优化线路',
    nodeId: 'hk-01',
    sourceMarket: '中国大陆',
    destinationMarket: '香港',
  },
};

test('auto renewal distinguishes wallet refusals and explicit disabled state', () => {
  assert.equal(business.autoRenewMessage({ autoRenew: false }), '未开启');
  assert.equal(business.autoRenewMessage({ autoRenew: true }), '流量耗尽时从余额续费');
  assert.match(business.autoRenewMessage({ autoRenew: true, autoRenewError: 'insufficient-balance' }), /余额不足/);
  assert.match(business.autoRenewMessage({ autoRenew: true, autoRenewError: 'wallet-frozen' }), /钱包已冻结/);
});

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function memoryStorage() {
  const values = new Map();
  return {
    getItem: key => values.has(key) ? values.get(key) : null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: key => values.delete(key),
  };
}

test('finance actions report into the visible finance status region', () => {
  assert.equal(typeof business.businessStatusSelector, 'function');
  assert.equal(business.businessStatusSelector('billing'), '#business-message');
  assert.equal(business.businessStatusSelector('finance'), '#finance-message');
});

test('wallet top-up accepts USD input and converts it to integer cents exactly', () => {
  assert.equal(typeof business.usdInputToCents, 'function');
  for (const [input, cents] of [['0.50', 50], ['5', 500], ['10.01', 1001], ['10000.00', 1000000]]) {
    assert.equal(business.usdInputToCents(input), cents);
  }
  for (const input of ['', '0.49', '1.001', '1e2', '-1', '10000.01']) {
    assert.throws(() => business.usdInputToCents(input), /USD/);
  }
  assert.equal(business.signedUsd(500), '+$5.00');
  assert.equal(business.signedUsd(-125), '-$1.25');
  assert.equal(business.signedUsd(0), '$0.00');
});

test('purchase cards expose route, independent limits, period, and USD price', () => {
  assert.equal(typeof business.planPurchaseCard, 'function');
  assert.deepEqual(business.planPurchaseCard(plan), {
    id: 'plan-hk-1000',
    offeringId: 'offering-hk',
    offeringName: '香港优化线路',
    route: '中国大陆 → 香港',
    name: '标准套餐',
    quota: '1,000 GB',
    bandwidth: '100 Mbps',
    period: '30 天',
    price: '$19.99',
    resourceMode: '共享资源',
  });
});

test('line filter keeps only cards from the selected offering', () => {
  assert.equal(typeof business.filterPurchasePlans, 'function');
  const second = { ...plan, id: 'plan-sg', offering: { ...plan.offering, id: 'offering-sg', name: '新加坡线路' } };
  assert.deepEqual(business.filterPurchasePlans([plan, second], 'offering-hk').map(item => item.id), ['plan-hk-1000']);
  assert.deepEqual(business.filterPurchasePlans([plan, second], '').map(item => item.id), ['plan-hk-1000', 'plan-sg']);
});

test('configuration dialog binds the selected card to the current rule', () => {
  assert.equal(typeof business.purchaseDialogSelection, 'function');
  assert.deepEqual(business.purchaseDialogSelection(plan, { id: 'rule-01', name: '客户官网', nodeId: 'hk-01' }), {
    body: { ruleId: 'rule-01', priceVersionId: 'plan-hk-1000' },
    title: '配置标准套餐',
    rule: '客户官网 · hk-01',
    plan: '香港优化线路 · 1,000 GB · 100 Mbps · 30 天',
    price: '$19.99',
    priceCents: 1999,
  });
});

test('purchase dialog can buy an unbound package before a rule exists', () => {
  assert.deepEqual(business.purchaseDialogSelection(plan), {
    body: { ruleId: '', priceVersionId: 'plan-hk-1000' },
    title: '购买标准套餐',
    rule: '香港优化线路 · hk-01',
    plan: '香港优化线路 · 1,000 GB · 100 Mbps · 30 天',
    price: '$19.99',
    priceCents: 1999,
  });
});

test('payment page offers balance purchase and only enables a configured external channel', () => {
  assert.equal(typeof business.paymentCheckoutPresentation, 'function');
  const selection = business.purchaseDialogSelection(plan, { id: 'rule-01', name: '客户官网', nodeId: 'hk-01' });

  assert.deepEqual(business.paymentCheckoutPresentation(selection, { initialized: true, balanceCents: 2500, spendingFrozen: false }, { enabled: false }), {
    amount: '$19.99',
    balance: '$25.00',
    walletContribution: '$19.99',
    balanceAfter: '$5.01',
    externalAmount: '$0.00',
    externalAmountCents: 0,
    externalRemainder: '$0.00',
    balanceEnabled: true,
    balanceDetail: '支付后钱包余额 $5.01',
    externalEnabled: false,
    externalDetail: '钱包余额足够，可直接完成购买。',
  });

  assert.deepEqual(business.paymentCheckoutPresentation(selection, { initialized: true, balanceCents: 500, spendingFrozen: false }, { enabled: true }), {
    amount: '$19.99',
    balance: '$5.00',
    walletContribution: '$5.00',
    balanceAfter: '$0.00',
    externalAmount: '$14.99',
    externalAmountCents: 1499,
    externalRemainder: '$0.00',
    balanceEnabled: false,
    balanceDetail: '余额不足，还差 $14.99',
    externalEnabled: true,
    externalDetail: '将使用钱包 $5.00，Stripe 支付 $14.99；验签到账后自动购买套餐。',
  });
});

test('direct checkout explains Stripe minimum-charge excess retained in wallet', () => {
  const selection = business.purchaseDialogSelection(plan);
  const view = business.paymentCheckoutPresentation(selection, { initialized: true, balanceCents: 1998, spendingFrozen: false }, { enabled: true });
  assert.equal(view.walletContribution, '$19.98');
  assert.equal(view.externalAmount, '$0.50');
  assert.equal(view.externalRemainder, '$0.49');
  assert.equal(view.externalEnabled, true);
  assert.equal(view.externalDetail, 'Stripe 最低支付 $0.50，其中 $0.01 补足套餐，剩余 $0.49 留在钱包。');
});

test('payment page blocks every payment method while wallet spending is frozen', () => {
  assert.equal(typeof business.paymentCheckoutPresentation, 'function');
  const selection = business.purchaseDialogSelection(plan, { id: 'rule-01', name: '客户官网', nodeId: 'hk-01' });
  const view = business.paymentCheckoutPresentation(selection, { initialized: true, balanceCents: 2500, spendingFrozen: true }, { enabled: true });
  assert.equal(view.balanceEnabled, false);
  assert.equal(view.externalEnabled, false);
  assert.equal(view.balanceDetail, '钱包支出已冻结，请联系平台完成对账。');
  assert.equal(view.externalDetail, '钱包支出已冻结，不能发起新的支付。');
});

test('TRON invoice renders exact network, address, amount and truthful server state', () => {
  assert.equal(typeof business.tronInvoicePresentation, 'function');
  const base = {
    id: 'order-1', network: 'TRON', asset: 'USDT', amountBaseUnits: 19990000,
    collectionAddress: 'TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw', state: 'pending', purchaseState: 'pending',
		expiresAt: '2026-11-12T13:30:00.000Z', serverTime: '2026-11-12T13:00:00.000Z',
  };
  assert.deepEqual(business.tronInvoicePresentation(base), {
    id: 'order-1', network: 'TRON (TRC-20)', asset: 'USDT', amount: '19.990000 USDT',
    amountValue: '19.990000', address: 'TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw', canClaim: true,
		qrPath: '/api/payments/tron/orders/order-1/qr',
    headline: '等待付款', success: false,
    status: '等待链上转账；固化后将自动识别并入账。', terminal: false,
  });
  assert.equal(business.tronInvoicePresentation({ ...base, state: 'confirming' }).status, '交易已提交，正在等待 TRON 固化确认。');
  assert.equal(business.tronInvoicePresentation({ ...base, state: 'needs-review' }).status, '交易需要人工核对，钱包尚未入账。');
  assert.equal(business.tronInvoicePresentation({ ...base, state: 'credited', purchaseState: 'failed' }).status, '充值已入账；套餐购买未完成，金额保留在钱包。');
  const credited = business.tronInvoicePresentation({ ...base, state: 'credited', purchaseState: 'purchased' });
  assert.equal(credited.status, '充值已入账，套餐购买完成。');
  assert.equal(credited.headline, '充值成功');
  assert.equal(credited.success, true);
});

test('TRON status polling stops after the server confirms wallet credit', async () => {
  assert.equal(typeof business.createTronStatusPoller, 'function');
  const scheduled = [];
  const observed = [];
  const responses = [
    { id: 'order-1', state: 'confirming' },
    { id: 'order-1', state: 'credited', amountBaseUnits: 19990000 },
  ];
  const poller = business.createTronStatusPoller({
    load: async () => responses.shift(),
    onOrder: async order => observed.push(order.state),
    schedule: callback => { scheduled.push(callback); return callback; },
    cancel: () => {},
    delay: 5000,
  });
  poller.start({ id: 'order-1', state: 'pending' });
  assert.equal(scheduled.length, 1);
  await scheduled.shift()();
  assert.deepEqual(observed, ['confirming']);
  assert.equal(scheduled.length, 1);
  await scheduled.shift()();
  assert.deepEqual(observed, ['confirming', 'credited']);
  assert.equal(scheduled.length, 0);
});

test('closing TRON payment invalidates an in-flight status response', async () => {
  let scheduled;
  let resolveLoad;
  const observed = [];
  const poller = business.createTronStatusPoller({
    load: () => new Promise(resolve => { resolveLoad = resolve; }),
    onOrder: order => observed.push(order.state),
    schedule: callback => { scheduled = callback; return callback; },
    cancel: () => {},
  });
  poller.start({ id: 'order-1', state: 'pending' });
  const pending = scheduled();
  poller.stop();
  resolveLoad({ id: 'order-1', state: 'credited' });
  await pending;
  assert.deepEqual(observed, []);
});

test('TRON countdown uses server time, clamps at zero and disables new payment', () => {
	assert.equal(typeof business.tronExpiryPresentation, 'function');
	const order = { state: 'pending', expiresAt: '2026-11-12T13:30:00.000Z', serverTime: '2026-11-12T13:00:00.000Z' };
	assert.deepEqual(business.tronExpiryPresentation(order, 0), { label: '30:00', seconds: 1800, expired: false, qrEnabled: true, copyEnabled: true });
	assert.deepEqual(business.tronExpiryPresentation(order, 1000), { label: '29:59', seconds: 1799, expired: false, qrEnabled: true, copyEnabled: true });
	assert.deepEqual(business.tronExpiryPresentation(order, 1800500), { label: '00:00', seconds: 0, expired: true, qrEnabled: false, copyEnabled: false });
	assert.deepEqual(business.tronExpiryPresentation({ ...order, state: 'credited' }, 0), { label: '30:00', seconds: 1800, expired: false, qrEnabled: false, copyEnabled: false });
});

test('TRON requests are fenced across close, logout and tenant changes', () => {
  const gate = business.createTronRequestGate();
  gate.setTenant('tenant-a');
  const tenantA = gate.begin();
  assert.equal(gate.isCurrent(tenantA), true);
  gate.invalidate();
  assert.equal(gate.isCurrent(tenantA), false);
  const closed = gate.begin();
  gate.setTenant('tenant-b');
  assert.equal(gate.isCurrent(closed), false);
  const tenantB = gate.begin();
  assert.equal(gate.isCurrent(tenantB), true);
  gate.reset();
  assert.equal(gate.isCurrent(tenantB), false);
});

test('checkout preparation refreshes payment availability and reports a refresh failure without blocking', async () => {
  assert.equal(typeof business.preparePaymentCheckout, 'function');
  const calls = [];
  assert.equal(await business.preparePaymentCheckout(async () => { calls.push('refresh'); }), true);
  assert.deepEqual(calls, ['refresh']);

  const failure = new Error('temporary refresh failure');
  const reported = [];
  assert.equal(await business.preparePaymentCheckout(async () => { throw failure; }, error => reported.push(error)), false);
  assert.deepEqual(reported, [failure]);
});

test('authorized customers can inspect checkout even when wallet spending is frozen', () => {
  assert.equal(typeof business.canOpenPackageCheckout, 'function');
  assert.equal(business.canOpenPackageCheckout({ role: 'owner' }, [{ id: 'rule-01' }], { spendingFrozen: true }), true);
  assert.equal(business.canOpenPackageCheckout({ role: 'viewer' }, [{ id: 'rule-01' }], { spendingFrozen: false }), false);
  assert.equal(business.canOpenPackageCheckout({ role: 'admin' }, [], { spendingFrozen: false }), true);
});

test('external payment submits the selected package and keeps one idempotency key on retry', async () => {
  assert.equal(typeof business.createExternalCheckoutSession, 'function');
  const calls = [];
  let attempts = 0;
  const checkout = business.createExternalCheckoutSession({
    createKey: () => 'external-key-1',
    submit: async (body, key) => {
      calls.push({ body, key });
      attempts += 1;
      if (attempts === 1) throw new Error('result unknown');
      return { checkoutUrl: 'https://checkout.stripe.com/c/pay/test' };
    },
  });
  const selection = business.purchaseDialogSelection(plan, { id: 'rule-01', name: '客户官网', nodeId: 'hk-01' });

  await assert.rejects(() => checkout.submit(selection), /result unknown/);
  assert.deepEqual(await checkout.submit(selection), { checkoutUrl: 'https://checkout.stripe.com/c/pay/test' });
  assert.deepEqual(calls, [
    { body: { ruleId: 'rule-01', priceVersionId: 'plan-hk-1000' }, key: 'external-key-1' },
    { body: { ruleId: 'rule-01', priceVersionId: 'plan-hk-1000' }, key: 'external-key-1' },
  ]);
});

test('external payment restores tenant-scoped idempotency after redirect and A-B-A selection changes', async () => {
  assert.equal(typeof business.createExternalCheckoutSession, 'function');
  const storage = memoryStorage();
  let keyNumber = 0;
  let now = 1000;
  const originalNow = Date.now;
  Date.now = () => now++;
  const calls = [];
  const create = tenantId => business.createExternalCheckoutSession({
    createKey: () => `retry-key-${++keyNumber}`,
    submit: async (body, key) => { calls.push({ tenantId, body, key }); return { id: `order-${key}` }; },
    storage,
    tenantId,
  });
  const selectionA = business.purchaseDialogSelection(plan, { id: 'rule-a', name: '规则 A', nodeId: 'hk-01' });
  const selectionB = business.purchaseDialogSelection({ ...plan, id: 'plan-b' }, { id: 'rule-b', name: '规则 B', nodeId: 'hk-01' });

  try {
    await create('tenant-a').submit(selectionA);
    const reloaded = create('tenant-a');
    await reloaded.submit(selectionB);
    await reloaded.submit(selectionA);
    await create('tenant-b').submit(selectionA);

    assert.deepEqual(calls.map(call => call.key), ['retry-key-1', 'retry-key-2', 'retry-key-1', 'retry-key-3']);
    assert.deepEqual(create('tenant-a').pendingSelection(), selectionA);
  } finally {
    Date.now = originalNow;
  }
});

test('external payment marks a response stale after checkout navigation is invalidated', async () => {
  assert.equal(typeof business.createExternalCheckoutSession, 'function');
  const request = deferred();
  const checkout = business.createExternalCheckoutSession({
    createKey: () => 'external-key',
    submit: () => request.promise,
    storage: memoryStorage(),
    tenantId: 'tenant-a',
  });
  const selection = business.purchaseDialogSelection(plan, { id: 'rule-01', name: '客户官网', nodeId: 'hk-01' });

  const pending = checkout.submit(selection);
  assert.equal(checkout.isSubmitting(), true);
  checkout.invalidate();
  request.resolve({ checkoutUrl: 'https://checkout.stripe.com/c/pay/stale' });
  assert.deepEqual(await pending, { stale: true });
  assert.equal(checkout.isSubmitting(), false);
});

test('external checkout reconciles exact and recent tenant orders without trusting return parameters', async () => {
  assert.equal(typeof business.planPaymentOrderPresentation, 'function');
  assert.deepEqual(business.planPaymentOrderPresentation({ state: 'pending', purchaseState: 'pending' }), {
    complete: false,
    clear: false,
    message: '支付处理中，请等待 Stripe 验签回调。',
  });
  assert.match(business.planPaymentOrderPresentation({ state: 'credited', purchaseState: 'failed', purchaseError: 'unavailable' }).message, /已留在企业钱包/);
  assert.equal(business.planPaymentOrderPresentation({ state: 'credited', purchaseState: 'purchased', subscriptionId: 'sub-1' }).complete, true);
  assert.match(business.planPaymentOrderPresentation({ state: 'needs-review', purchaseState: 'pending' }).message, /人工对账/);
  const purchasedReview = business.planPaymentOrderPresentation({ state: 'needs-review', purchaseState: 'purchased', subscriptionId: 'sub-1' });
  assert.equal(purchasedReview.complete, false);
  assert.equal(purchasedReview.clear, false);
  assert.match(purchasedReview.message, /套餐仍已购买/);
  assert.match(purchasedReview.message, /人工对账/);
  assert.match(business.planPaymentOrderPresentation({ state: 'expired', purchaseState: 'failed', purchaseError: 'payment-expired' }).message, /已过期/);

  const storage = memoryStorage();
  const selection = business.purchaseDialogSelection(plan);
  const checkout = business.createExternalCheckoutSession({
    createKey: () => 'reconcile-key',
    submit: async () => ({ id: 'order-local', checkoutUrl: 'https://checkout.stripe.com/c/pay/test' }),
    storage,
    tenantId: 'tenant-a',
  });
  await checkout.submit(selection);
  const exactCalls = [];
  const exact = await checkout.reconcile(async id => {
    exactCalls.push(id);
    return { id, state: 'pending', purchaseState: 'pending', ruleId: '', priceVersionId: plan.id };
  }, async () => []);
  assert.deepEqual(exactCalls, ['order-local']);
  assert.equal(exact.order.id, 'order-local');
  assert.deepEqual(exact.selection, selection);

  const crossDevice = business.createExternalCheckoutSession({ createKey: () => 'unused-key', submit: async () => ({}), storage: memoryStorage(), tenantId: 'tenant-a' });
  const recent = await crossDevice.reconcile(async () => { throw new Error('no local id'); }, async () => [{ id: 'order-recent', state: 'credited', purchaseState: 'purchased', priceVersionId: plan.id }]);
  assert.equal(recent.order.id, 'order-recent');
  assert.equal(recent.selection, undefined);
});

test('Stripe redirect requires renewed confirmation when the server amount exceeds the stale wallet quote', () => {
  assert.equal(typeof business.stripeCheckoutConfirmation, 'function');
  const selection = business.purchaseDialogSelection(plan);
  const staleView = business.paymentCheckoutPresentation(selection, { balanceCents: 500, spendingFrozen: false }, { stripe: true });
  assert.deepEqual(business.stripeCheckoutConfirmation(staleView, { amountCents: 1499 }), { required: false, displayed: '$14.99', actual: '$14.99', message: '' });
  const changed = business.stripeCheckoutConfirmation(staleView, { amountCents: 1999 });
  assert.equal(changed.required, true);
  assert.equal(changed.displayed, '$14.99');
  assert.equal(changed.actual, '$19.99');
  assert.match(changed.message, /服务端重新计算/);
});

test('latest plan load wins when an older request fails later', async () => {
  assert.equal(typeof business.createLatestPlanLoader, 'function');
  const first = deferred();
  const second = deferred();
  const loading = [];
  const successes = [];
  const errors = [];
  const loader = business.createLatestPlanLoader({
    load: ruleId => (ruleId === 'rule-a' ? first.promise : second.promise),
    onLoading: ruleId => loading.push(ruleId),
    onSuccess: (data, ruleId) => successes.push([ruleId, data.items]),
    onError: (error, ruleId) => errors.push([ruleId, error.message]),
  });

  const firstLoad = loader('rule-a');
  const secondLoad = loader('rule-b');
  assert.deepEqual(loading, ['rule-a', 'rule-b']);

  second.resolve({ items: ['plan-b'] });
  assert.deepEqual(await secondLoad, { stale: false, data: { items: ['plan-b'] } });
  first.reject(new Error('old request failed'));
  assert.deepEqual(await firstLoad, { stale: true });
  assert.deepEqual(successes, [['rule-b', ['plan-b']]]);
  assert.deepEqual(errors, []);
});

test('purchase session blocks close and replacement while submitting', async () => {
  assert.equal(typeof business.createPurchaseSession, 'function');
  const request = deferred();
  const busy = [];
  const successes = [];
  const session = business.createPurchaseSession({
    createKey: () => 'purchase-key',
    submit: () => request.promise,
    onBusy: value => busy.push(value),
    onSuccess: (_result, selection) => successes.push(selection.title),
  });
  const firstSelection = { title: '套餐 A', body: { ruleId: 'rule-a', priceVersionId: 'plan-a' } };
  const secondSelection = { title: '套餐 B', body: { ruleId: 'rule-b', priceVersionId: 'plan-b' } };

  assert.equal(typeof session.current, 'function');
  assert.equal(session.open(firstSelection), true);
  assert.deepEqual(session.current(), firstSelection);
  const submission = session.submit();
  assert.equal(session.isSubmitting(), true);
  assert.equal(session.close(), false);
  assert.equal(session.open(secondSelection), false);

  request.resolve({ id: 'subscription-a' });
  assert.deepEqual(await submission, { ok: true, result: { id: 'subscription-a' } });
  assert.deepEqual(successes, ['套餐 A']);
  assert.deepEqual(busy, [true, false]);
  assert.equal(session.isSubmitting(), false);
});

test('purchase session reset invalidates tenant checkout state and pending results', async () => {
  const request = deferred();
  const session = business.createPurchaseSession({
    createKey: () => 'purchase-key',
    submit: () => request.promise,
  });
  session.open({ title: '套餐 A', body: { ruleId: 'rule-a', priceVersionId: 'plan-a' } });
  const pending = session.submit();
  assert.equal(typeof session.reset, 'function');
  session.reset();
  assert.equal(session.current(), undefined);
  request.resolve({ id: 'subscription-a' });
  assert.deepEqual(await pending, { stale: true });
});

test('tenant refresh reset prevents an old account response from replacing the new account', async () => {
  assert.equal(typeof business.createTenantRefreshGate, 'function');
  const first = deferred();
  const second = deferred();
  const committed = [];
  const gate = business.createTenantRefreshGate();

  const tenantA = gate.run('tenant-a', () => first.promise, value => committed.push(value));
  gate.reset();
  const tenantB = gate.run('tenant-b', () => second.promise, value => committed.push(value));
  second.resolve('tenant-b-wallet');
  assert.deepEqual(await tenantB, { stale: false, value: 'tenant-b-wallet' });
  first.resolve('tenant-a-wallet');
  assert.deepEqual(await tenantA, { stale: true });
  assert.deepEqual(committed, ['tenant-b-wallet']);
});

test('purchase retry keeps the same idempotency key for one dialog session', async () => {
  assert.equal(typeof business.createPurchaseSession, 'function');
  const keys = [];
  let attempts = 0;
  const errors = [];
  const session = business.createPurchaseSession({
    createKey: () => 'stable-key',
    submit: async (_body, key) => {
      keys.push(key);
      attempts += 1;
      if (attempts === 1) throw new Error('temporary failure');
      return { id: 'subscription-a' };
    },
    onError: error => errors.push(error.message),
  });
  session.open({ title: '套餐 A', body: { ruleId: 'rule-a', priceVersionId: 'plan-a' } });

  assert.equal((await session.submit()).ok, false);
  assert.equal((await session.submit()).ok, true);
  assert.deepEqual(keys, ['stable-key', 'stable-key']);
  assert.deepEqual(errors, ['temporary failure']);
});

test('purchase retry keeps its idempotency key after closing and reopening the same selection', async () => {
  assert.equal(typeof business.createPurchaseSession, 'function');
  const firstRequest = deferred();
  const keys = [];
  let keyNumber = 0;
  let attempts = 0;
  const selection = { title: '套餐 A', body: { ruleId: 'rule-a', priceVersionId: 'plan-a' } };
  const session = business.createPurchaseSession({
    createKey: () => `key-${++keyNumber}`,
    submit: async (_body, key) => {
      keys.push(key);
      attempts += 1;
      if (attempts === 1) return firstRequest.promise;
      return { id: 'subscription-a' };
    },
  });

  session.open(selection);
  const firstSubmission = session.submit();
  firstRequest.reject(new Error('result unknown'));
  assert.equal((await firstSubmission).ok, false);
  assert.equal(session.close(), true);
  assert.equal(session.open(selection), true);
  assert.equal((await session.submit()).ok, true);
  assert.deepEqual(keys, ['key-1', 'key-1']);
});
