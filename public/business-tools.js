import { offeringRouteLabel } from './plan-catalog.js';
import { resourceModeLabel } from './resource-mode.js';

const $ = selector => document.querySelector(selector);
function option(select, value, text) { const item = document.createElement('option'); item.value = value; item.textContent = text; select.append(item); }

export function businessStatusSelector(area) {
  return area === 'finance' ? '#finance-message' : '#business-message';
}

function setBusinessStatus(area, message) {
  const target = $(businessStatusSelector(area));
  if (target) target.textContent = message;
}

let topUp;
let canPurchase = false;
let canManagePurchase = false;
let purchasePlans = [];
let purchaseRules = [];
let selectedOffering = '';
let catalogRuleId = '';
let walletState = { initialized: false, balanceCents: 0, spendingFrozen: false };
let paymentState = { enabled: false, stripe: false, tron: false };
let planCatalogAPI;
let planCatalogLoader;
let purchaseSession;
let externalCheckoutSession;
let tronCheckout;
let activeTronOrder;
let tronStatusPoller;
let tronCountdownTimer;
let tronCountdownStartedAt = 0;
let tronExpiryRefreshRequested = false;
let navigateWorkspace = view => { location.hash = `#/${view}`; };
let suppressPurchaseDialogClose = false;
const businessRefreshGate = createTenantRefreshGate();
const tronRequestGate = createTronRequestGate();

function beginTronRequest() { return tronRequestGate.begin(); }
function currentTronRequest(request) { return tronRequestGate.isCurrent(request); }
function tronMonotonicNow() { return globalThis.performance?.now?.() ?? Date.now(); }
function stopTronCountdown() {
  if (tronCountdownTimer) clearInterval(tronCountdownTimer);
  tronCountdownTimer = undefined;
}
function invalidateTronRequests(close = false) {
	tronRequestGate.invalidate();
  stopTronCountdown();
  tronStatusPoller?.stop();
  if (close) {
    activeTronOrder = undefined;
    const dialog = typeof document === 'undefined' ? undefined : $('#tron-payment-dialog');
    if (dialog?.open) dialog.close();
  }
}

export function createTronRequestGate() {
  let generation = 0;
  let tenantId = '';
  return {
    setTenant(nextTenantId) {
      nextTenantId ||= '';
      if (tenantId === nextTenantId) return false;
      tenantId = nextTenantId;
      generation += 1;
      return true;
    },
    begin() { return { generation: ++generation, tenantId }; },
    isCurrent(request) { return Boolean(tenantId) && request?.generation === generation && request.tenantId === tenantId; },
    invalidate() { generation += 1; },
    reset() { tenantId = ''; generation += 1; },
  };
}

export function createTronStatusPoller({
  load,
  onOrder = async () => {},
  onError = () => {},
  schedule = globalThis.setTimeout,
  cancel = globalThis.clearTimeout,
  delay = 5000,
}) {
  let generation = 0;
  let timer;
  const watchable = order => ['pending', 'confirming'].includes(order?.state) && Boolean(order?.id);
  const clear = () => {
    if (timer !== undefined) cancel(timer);
    timer = undefined;
  };
  const stop = () => {
    generation += 1;
    clear();
  };
  const start = order => {
    stop();
    if (!watchable(order)) return;
    const current = ++generation;
    const orderID = order.id;
    const poll = async () => {
      timer = undefined;
      try {
        const next = await load(orderID);
        if (current !== generation) return;
        await onOrder(next);
        if (current !== generation || !watchable(next)) return;
      } catch (error) {
        if (current !== generation) return;
        onError(error);
      }
      if (current === generation) timer = schedule(poll, delay);
    };
    timer = schedule(poll, delay);
  };
  return { start, stop };
}

export function createLatestPlanLoader({ load, onLoading = () => {}, onSuccess = () => {}, onError = () => {} }) {
  let generation = 0;
  const loader = async key => {
    const current = ++generation;
    onLoading(key, current);
    try {
      const data = await load(key);
      if (current !== generation) return { stale: true };
      onSuccess(data, key, current);
      return { stale: false, data };
    } catch (error) {
      if (current !== generation) return { stale: true };
      onError(error, key, current);
      return { stale: false, error };
    }
  };
  loader.invalidate = () => { generation += 1; };
  return loader;
}

export function createTenantRefreshGate() {
  let generation = 0;
  let tenantId = '';
  const begin = nextTenantId => ({ generation: ++generation, tenantId: tenantId = nextTenantId || '' });
  const isCurrent = request => Boolean(request) && request.generation === generation && request.tenantId === tenantId;
  return {
    begin,
    isCurrent,
    current() {
      return tenantId ? { generation, tenantId } : undefined;
    },
    reset() {
      generation += 1;
      tenantId = '';
    },
    async run(nextTenantId, load, commit = () => {}) {
      const request = begin(nextTenantId);
      try {
        const value = await load();
        if (!isCurrent(request)) return { stale: true };
        await commit(value);
        if (!isCurrent(request)) return { stale: true };
        return { stale: false, value };
      } catch (error) {
        if (!isCurrent(request)) return { stale: true };
        throw error;
      }
    },
  };
}

export function createPurchaseSession({
  createKey,
  submit,
  afterSubmit = async () => {},
  onBusy = () => {},
  onSuccess = () => {},
  onError = () => {},
}) {
  let current;
  let retry;
  return {
    open(selection) {
      if (current?.submitting) return false;
      const serialized = JSON.stringify(selection.body);
      current = {
        selection,
        serialized,
        key: retry?.serialized === serialized ? retry.key : undefined,
        submitting: false,
      };
      return true;
    },
    close() {
      if (current?.submitting) return false;
      current = undefined;
      return true;
    },
    isSubmitting() {
      return Boolean(current?.submitting);
    },
    current() {
      return current?.selection;
    },
    reset() {
      current = undefined;
      retry = undefined;
    },
    async submit() {
      const session = current;
      if (!session || session.submitting) return { ignored: true };
      session.submitting = true;
      session.key ||= createKey();
      retry = { serialized: session.serialized, key: session.key };
      onBusy(true, session.selection);
      let result;
      try {
        result = await submit(session.selection.body, session.key);
        if (current !== session) return { stale: true };
        await afterSubmit(result, session.selection);
        if (current !== session) return { stale: true };
      } catch (error) {
        if (current !== session) return { stale: true };
        session.submitting = false;
        onBusy(false, session.selection);
        onError(error, session.selection);
        return { ok: false, error };
      }
      if (retry?.serialized === session.serialized && retry.key === session.key) retry = undefined;
      current = undefined;
      session.submitting = false;
      onBusy(false, session.selection);
      onSuccess(result, session.selection);
      return { ok: true, result };
    },
  };
}

export function planPurchaseCard(plan) {
  return {
    id: plan.id,
    offeringId: plan.offering.id,
    offeringName: plan.offering.name,
    route: offeringRouteLabel(plan.offering),
    name: plan.name,
    quota: `${(plan.quotaBytes / 1e9).toLocaleString()} GB`,
    bandwidth: `${plan.bandwidthMbps.toLocaleString()} Mbps`,
    period: `${(plan.periodSeconds / 86400).toLocaleString()} 天`,
    price: `$${(plan.priceCents / 100).toFixed(2)}`,
    resourceMode: resourceModeLabel(plan.resourceMode),
  };
}

export function filterPurchasePlans(plans, offeringId) {
  return offeringId ? plans.filter(plan => plan.offering?.id === offeringId) : plans;
}

export function purchaseDialogSelection(plan, rule) {
  const card = planPurchaseCard(plan);
  return {
    body: { ruleId: rule?.id || '', priceVersionId: plan.id },
    title: rule ? `配置${plan.name}` : `购买${plan.name}`,
    rule: rule ? `${rule.name} · ${rule.nodeId}` : `${card.offeringName} · ${plan.offering?.nodeId || '待选择入口'}`,
    plan: `${card.offeringName} · ${card.quota} · ${card.bandwidth} · ${card.period}`,
    price: card.price,
    priceCents: plan.priceCents,
  };
}

function usd(cents) {
  return `$${(Math.max(0, Number(cents) || 0) / 100).toFixed(2)}`;
}

export function usdInputToCents(value) {
  const input = String(value ?? '').trim();
  if (!/^\d+(?:\.\d{1,2})?$/.test(input)) {
    throw new Error('充值金额必须是最多两位小数的 USD 金额。');
  }
  const [whole, fraction = ''] = input.split('.');
  const cents = BigInt(whole) * 100n + BigInt(fraction.padEnd(2, '0'));
  if (cents < 50n || cents > 1000000n) {
    throw new Error('充值金额必须在 USD 0.50–10,000.00 之间。');
  }
  return Number(cents);
}

export function signedUsd(cents) {
  const amount = Number(cents) || 0;
  const sign = amount > 0 ? '+' : amount < 0 ? '-' : '';
  return `${sign}$${(Math.abs(amount) / 100).toFixed(2)}`;
}

export function paymentCheckoutPresentation(selection, wallet, payment) {
  const priceCents = Math.max(0, Number(selection?.priceCents) || 0);
  const balanceCents = Math.max(0, Number(wallet?.balanceCents) || 0);
  const frozen = wallet?.spendingFrozen === true;
  const enough = balanceCents >= priceCents;
  const stripeEnabled = payment?.stripe === true || (payment?.stripe === undefined && payment?.enabled === true);
  const walletContributionCents = Math.min(balanceCents, priceCents);
  const shortfallCents = Math.max(0, priceCents - balanceCents);
  const externalAmountCents = shortfallCents > 0 ? Math.max(50, shortfallCents) : 0;
  const externalRemainderCents = externalAmountCents - shortfallCents;
  let externalDetail = '尚未启用其他支付方式。';
  if (enough) {
    externalDetail = '钱包余额足够，可直接完成购买。';
  } else if (stripeEnabled && externalRemainderCents > 0) {
    externalDetail = `Stripe 最低支付 ${usd(externalAmountCents)}，其中 ${usd(shortfallCents)} 补足套餐，剩余 ${usd(externalRemainderCents)} 留在钱包。`;
  } else if (stripeEnabled) {
    externalDetail = `将使用钱包 ${usd(walletContributionCents)}，Stripe 支付 ${usd(externalAmountCents)}；验签到账后自动购买套餐。`;
  }
  return {
    amount: usd(priceCents),
    balance: usd(balanceCents),
    walletContribution: usd(walletContributionCents),
    balanceAfter: usd(enough ? balanceCents - priceCents : 0),
    externalAmount: usd(externalAmountCents),
    externalAmountCents,
    externalRemainder: usd(externalRemainderCents),
    balanceEnabled: !frozen && priceCents > 0 && enough,
    balanceDetail: frozen
      ? '钱包支出已冻结，请联系平台完成对账。'
      : enough
        ? `支付后钱包余额 ${usd(balanceCents - priceCents)}`
        : `余额不足，还差 ${usd(priceCents - balanceCents)}`,
    externalEnabled: !frozen && priceCents > 0 && !enough && stripeEnabled,
    externalDetail: frozen
      ? '钱包支出已冻结，不能发起新的支付。'
      : externalDetail,
  };
}

export function tronInvoicePresentation(order) {
  const state = order?.state || '';
  const purchaseState = order?.purchaseState || 'none';
  const headline = ({ pending: '等待付款', confirming: '正在确认', credited: '充值成功', 'needs-review': '待人工核对', expired: '订单已过期' })[state] || '订单状态未知';
  let status = '订单状态未知，请刷新。';
  if (state === 'pending') status = '等待链上转账；固化后将自动识别并入账。';
  if (state === 'confirming') status = '交易已提交，正在等待 TRON 固化确认。';
  if (state === 'needs-review') status = '交易需要人工核对，钱包尚未入账。';
  if (state === 'expired') status = order?.claimAllowed
    ? '转账窗口已过期；若已在有效期内转账，仍可提交交易哈希核验。'
    : '订单及核验宽限期均已结束，请联系平台人工核对。';
  if (state === 'credited') status = purchaseState === 'purchased'
    ? '充值已入账，套餐购买完成。'
    : purchaseState === 'failed'
      ? '充值已入账；套餐购买未完成，金额保留在钱包。'
      : '充值已入账。';
  return {
    id: order?.id || '',
    network: 'TRON (TRC-20)',
    asset: 'USDT',
    amount: `${(Math.max(0, Number(order?.amountBaseUnits) || 0) / 1e6).toFixed(6)} USDT`,
    amountValue: (Math.max(0, Number(order?.amountBaseUnits) || 0) / 1e6).toFixed(6),
    address: order?.collectionAddress || '',
    canClaim: order?.claimAllowed ?? state === 'pending',
    qrPath: order?.id ? `/api/payments/tron/orders/${order.id}/qr` : '',
    headline,
    success: state === 'credited',
    status,
    terminal: ['credited', 'needs-review', 'expired'].includes(state),
  };
}

export function tronExpiryPresentation(order, elapsedMs = 0) {
  const expiresAt = Date.parse(order?.expiresAt || '');
  const parsedServerTime = Date.parse(order?.serverTime || '');
  const serverTime = Number.isFinite(parsedServerTime) ? parsedServerTime : Date.now();
  const remainingMs = Number.isFinite(expiresAt) ? expiresAt - serverTime - Math.max(0, Number(elapsedMs) || 0) : 0;
  const seconds = Math.max(0, Math.ceil(remainingMs / 1000));
  const minutesPart = Math.floor(seconds / 60);
  const secondsPart = seconds % 60;
  const expired = seconds === 0;
  return {
    label: `${String(minutesPart).padStart(2, '0')}:${String(secondsPart).padStart(2, '0')}`,
    seconds,
    expired,
    qrEnabled: order?.state === 'pending' && !expired,
    copyEnabled: order?.state === 'pending' && !expired,
  };
}

function updateTronCountdown() {
  if (!activeTronOrder) return stopTronCountdown();
  const expiry = tronExpiryPresentation(activeTronOrder, tronMonotonicNow() - tronCountdownStartedAt);
  const countdown = $('#tron-payment-countdown');
  const stage = $('#tron-payment-qr-stage');
  const image = $('#tron-payment-qr');
  if (countdown) countdown.textContent = expiry.label;
  if (stage) stage.hidden = !expiry.qrEnabled;
  const copyAddress = $('#tron-payment-copy-address');
  if (copyAddress) copyAddress.disabled = !expiry.copyEnabled;
  if (!expiry.qrEnabled && image) image.removeAttribute('src');
  if (!expiry.expired) return;
  stopTronCountdown();
  if (activeTronOrder.state === 'pending') {
    $('#tron-payment-state').textContent = '转账窗口已结束；若已按时转账，系统仍会继续识别，也可手动提交哈希核验。';
    if (!tronExpiryRefreshRequested) {
      tronExpiryRefreshRequested = true;
      $('#tron-payment-refresh')?.click();
    }
  }
}

function startTronCountdown(order) {
  stopTronCountdown();
  if (order?.state !== 'pending') return;
  tronCountdownStartedAt = tronMonotonicNow();
  tronExpiryRefreshRequested = false;
  updateTronCountdown();
  if (tronExpiryPresentation(order, 0).seconds > 0 && order?.state === 'pending') {
    tronCountdownTimer = setInterval(updateTronCountdown, 1000);
  }
}

function renderTronInvoice(order) {
  activeTronOrder = order;
  const view = tronInvoicePresentation(order);
  $('#tron-payment-amount').textContent = view.amount;
  $('#tron-payment-address').textContent = view.address;
  const expiry = tronExpiryPresentation(order, 0);
  const qr = $('#tron-payment-qr');
  const qrStage = $('#tron-payment-qr-stage');
  const success = $('#tron-payment-success');
  const statusVisual = $('#tron-payment-status-visual');
  const nonPaymentState = !expiry.qrEnabled && !view.success;
  if (qr) {
    if (expiry.qrEnabled) qr.src = view.qrPath;
    else qr.removeAttribute('src');
  }
  qrStage.hidden = !expiry.qrEnabled;
  success.hidden = !view.success;
  statusVisual.hidden = !nonPaymentState;
  $('#tron-payment-status-headline').textContent = view.headline;
  $('#tron-payment-status-symbol').textContent = order?.state === 'confirming' ? '···' : '!';
  $('#tron-payment-success-detail').textContent = `${view.amount} 已记入企业钱包`;
  $('#tron-payment-qr-panel').dataset.state = order?.state || 'unknown';
  $('#tron-payment-copy-address').disabled = !expiry.copyEnabled;
  $('#tron-payment-validity-label').textContent = view.success ? '到账状态' : order?.state === 'confirming' ? '链上状态' : '有效时间';
  $('#tron-payment-countdown').textContent = view.success ? '已到账' : order?.state === 'confirming' ? '确认中' : expiry.label;
  $('#tron-payment-state').textContent = view.status;
  $('#tron-claim-form [name=txHash]').disabled = !view.canClaim;
  $('#tron-claim-submit').hidden = !view.canClaim;
  $('#tron-payment-recovery').hidden = !view.canClaim;
  $('#tron-payment-refresh').disabled = !view.id;
  $('#tron-payment-message').textContent = '';
  const dialog = $('#tron-payment-dialog');
  if (!dialog.open) dialog.showModal();
  startTronCountdown(order);
}

function showTronInvoice(order) {
  renderTronInvoice(order);
  tronStatusPoller?.start(order);
}

export function planPaymentOrderPresentation(order) {
  if (order?.state === 'needs-review') {
    if (order?.purchaseState === 'purchased' && order?.subscriptionId) {
      return { complete: false, clear: false, message: '套餐仍已购买并开始计时，但支付进入人工对账，钱包支出已冻结，请联系平台处理。' };
    }
    return { complete: false, clear: false, message: '支付需要人工对账，钱包支出已冻结，请联系平台处理。' };
  }
  if (order?.purchaseState === 'purchased' && order?.subscriptionId) {
    return { complete: true, clear: true, message: '套餐已购买并开始计时；最终状态来自已验签的支付回调。' };
  }
  if (order?.purchaseState === 'failed' && order?.state === 'credited') {
    const reason = ({ unavailable: '套餐或节点当前不可销售', unauthorized: '购买权限已变化', 'insufficient-balance': '支付期间钱包余额发生变化', 'wallet-frozen': '钱包支出已冻结', conflict: '当前套餐状态冲突' })[order.purchaseError] || '套餐未能自动开通';
    return { complete: false, clear: true, message: `${reason}；已验证的支付金额已留在企业钱包，可在条件恢复后使用余额购买。` };
  }
  if (order?.state === 'expired' || order?.purchaseError === 'payment-expired') {
    return { complete: false, clear: true, message: '支付订单已过期，未入账也未购买套餐，请重新发起。' };
  }
  if (order?.state === 'failed' || order?.purchaseError === 'payment-failed') {
    return { complete: false, clear: true, message: '支付未完成，未入账也未购买套餐，请重新发起。' };
  }
  return { complete: false, clear: false, message: '支付处理中，请等待 Stripe 验签回调。' };
}

export function stripeCheckoutConfirmation(view, order) {
  const displayedCents = Math.max(0, Number(view?.externalAmountCents) || 0);
  const actualCents = Math.max(0, Number(order?.amountCents) || 0);
  const required = displayedCents > 0 && actualCents > 0 && displayedCents !== actualCents;
  return {
    required,
    displayed: usd(displayedCents),
    actual: usd(actualCents),
    message: required
      ? `钱包余额已变化，服务端重新计算的 Stripe 支付金额为 ${usd(actualCents)}（页面原显示 ${usd(displayedCents)}）。确认按新金额继续？`
      : '',
  };
}

export async function preparePaymentCheckout(refresh, reportError = () => {}) {
  try {
    await refresh();
    return true;
  } catch (error) {
    reportError(error);
    return false;
  }
}

export function canOpenPackageCheckout(identity) {
	return ['owner', 'admin'].includes(identity?.role);
}

export function createExternalCheckoutSession({ createKey, submit, storage = globalThis.sessionStorage, tenantId = '' }) {
  const validKey = /^[a-zA-Z0-9._:-]{8,128}$/;
  let scope = tenantId;
  let generation = 0;
  let submitting = false;
  let volatileEntries = {};
  const storageKey = () => `reladock.external-checkout.${scope}`;

  function read() {
    if (!scope) return volatileEntries;
    if (typeof storage?.getItem !== 'function') return volatileEntries;
    try {
      const value = JSON.parse(storage?.getItem(storageKey()) || '{}');
      if (value && typeof value === 'object' && !Array.isArray(value)) {
        volatileEntries = value;
        return value;
      }
      return volatileEntries;
    } catch {
      return volatileEntries;
    }
  }

  function write(entries) {
    volatileEntries = entries;
    if (!scope || typeof storage?.setItem !== 'function') return;
    try { storage?.setItem(storageKey(), JSON.stringify(entries)); } catch { /* session storage may be unavailable */ }
  }

  function fingerprint(selection) {
    return JSON.stringify({ body: selection.body, priceCents: selection.priceCents });
  }

  function entryFor(selection) {
    const id = fingerprint(selection);
    const entries = read();
    let entry = entries[id];
    if (!entry || !validKey.test(entry.key || '')) {
      entry = { key: createKey(), selection, createdAt: Date.now() };
      entries[id] = entry;
      const ordered = Object.entries(entries).sort((a, b) => Number(b[1]?.createdAt || 0) - Number(a[1]?.createdAt || 0));
      write(Object.fromEntries(ordered.slice(0, 20)));
    }
    return { id, entry };
  }

  return {
    async submit(selection) {
      const request = ++generation;
      submitting = true;
      const { id, entry } = entryFor(selection);
      const entries = read();
      if (entries[id]) {
        const latest = Math.max(0, ...Object.values(entries).map(value => Number(value?.lastSubmittedAt || value?.createdAt || 0)));
        entries[id].lastSubmittedAt = Math.max(Date.now(), latest + 1);
        entries[id].selection = selection;
        write(entries);
      }
      try {
        const result = await submit(selection.body, entry.key);
        if (request !== generation) return { stale: true };
        const entries = read();
        if (entries[id]) {
          entries[id].orderId = result?.id || entries[id].orderId || '';
          entries[id].selection = selection;
          write(entries);
        }
        return result;
      } catch (error) {
        if (request !== generation) return { stale: true };
        throw error;
      } finally {
        if (request === generation) submitting = false;
      }
    },
    isSubmitting() {
      return submitting;
    },
    invalidate() {
      generation += 1;
      submitting = false;
    },
    setTenant(nextTenantId) {
      if (scope === nextTenantId) return;
      generation += 1;
      submitting = false;
      scope = nextTenantId || '';
      volatileEntries = {};
    },
    pendingSelection() {
      const entries = Object.values(read()).filter(entry => entry?.selection && validKey.test(entry.key || ''));
      entries.sort((a, b) => Number(b.lastSubmittedAt || b.createdAt || 0) - Number(a.lastSubmittedAt || a.createdAt || 0));
      return entries[0]?.selection;
    },
    async reconcile(loadOrder, listRecent) {
      const request = ++generation;
      const entries = read();
      const ordered = Object.entries(entries).sort((a, b) => Number(b[1]?.lastSubmittedAt || b[1]?.createdAt || 0) - Number(a[1]?.lastSubmittedAt || a[1]?.createdAt || 0));
      for (const [id, entry] of ordered) {
        if (!entry?.orderId) continue;
        try {
          const order = await loadOrder(entry.orderId);
          if (request !== generation) return { stale: true };
          const presentation = planPaymentOrderPresentation(order);
          if (presentation.clear && entry.selection) {
            delete entries[id];
            write(entries);
          }
          return { order, selection: entry.selection };
        } catch { /* recover through the tenant-scoped recent-order endpoint */ }
      }
      const recent = await listRecent();
      if (request !== generation) return { stale: true };
      const order = Array.isArray(recent) ? recent[0] : undefined;
      if (!order) return {};
      const match = ordered.find(([, entry]) => entry?.selection?.body?.ruleId === (order.ruleId || '') && entry?.selection?.body?.priceVersionId === order.priceVersionId);
      return { order, selection: match?.[1]?.selection };
    },
    clear(selection) {
      const entries = read();
      delete entries[fingerprint(selection)];
      write(entries);
    },
  };
}

function renderPaymentCheckout() {
  const selection = purchaseSession?.current();
  const empty = $('#checkout-empty');
  const content = $('#checkout-content');
  if (!selection) {
    empty.hidden = false;
    content.hidden = true;
    $('#checkout-status').textContent = '';
    return;
  }
  empty.hidden = true;
  content.hidden = false;
  const view = paymentCheckoutPresentation(selection, walletState, paymentState);
  const busy = purchaseSession.isSubmitting() || externalCheckoutSession?.isSubmitting();
  $('#checkout-rule').textContent = selection.rule;
  $('#checkout-plan').textContent = selection.plan;
  $('#checkout-amount').textContent = view.amount;
  $('#checkout-wallet-balance').textContent = view.balance;
  const externalAmount = $('#checkout-external-amount');
  if (externalAmount) externalAmount.textContent = view.externalAmount;
  $('#checkout-balance-detail').textContent = view.balanceDetail;
  $('#checkout-external-detail').textContent = view.externalDetail;
  $('#checkout-tron-detail').textContent = paymentState.tron ? '按订单精确金额转入 TRON 地址；固化且严格匹配后自动入账并购买。' : 'TRON USDT 收款尚未启用。';
  $('#checkout-back').disabled = busy;
  $('#checkout-balance-pay').disabled = !view.balanceEnabled || busy;
  $('#checkout-external-pay').disabled = !view.externalEnabled || busy;
  $('#checkout-tron-pay').disabled = walletState.spendingFrozen || !paymentState.tron || busy;
}

export function businessViewChanged(view) {
  if (view !== 'checkout' && externalCheckoutSession?.isSubmitting()) externalCheckoutSession.invalidate();
  if (view !== 'checkout' && activeTronOrder?.purpose === 'plan-purchase') invalidateTronRequests(true);
}

export function resetBusinessTools() {
  businessRefreshGate.reset();
  planCatalogLoader?.invalidate?.();
  topUp = undefined;
  walletState = { initialized: false, balanceCents: 0, spendingFrozen: false };
  paymentState = { enabled: false, stripe: false, tron: false };
  tronCheckout = undefined;
  activeTronOrder = undefined;
  tronRequestGate.reset();
  invalidateTronRequests(true);
  purchasePlans = [];
  purchaseRules = [];
  selectedOffering = '';
  catalogRuleId = '';
  canPurchase = false;
  canManagePurchase = false;
  purchaseSession?.reset();
  externalCheckoutSession?.setTenant('');
  suppressPurchaseDialogClose = false;
  if (typeof document !== 'undefined') {
    const dialog = $('#purchase-dialog');
    if (dialog?.open) dialog.close();
    const tronDialog = $('#tron-payment-dialog');
    if (tronDialog?.open) tronDialog.close();
    for (const selector of ['#purchase-dialog-title', '#purchase-dialog-rule', '#purchase-dialog-plan', '#purchase-dialog-price', '#purchase-dialog-status', '#checkout-status', '#business-message', '#finance-message']) {
      const element = $(selector);
      if (element) element.textContent = '';
    }
    const form = $('#purchase-form');
    form?.reset?.();
    const empty = $('#checkout-empty');
    const content = $('#checkout-content');
    if (empty) empty.hidden = false;
    if (content) content.hidden = true;
  }
}

function openPurchaseDialog(plan) {
  const currentPlan = purchasePlans.find(item => item.id === plan.id);
	if (!currentPlan || !canPurchase) return;
	const selection = purchaseDialogSelection(currentPlan);
  if (!purchaseSession?.open(selection)) return;
  $('#purchase-dialog-title').textContent = selection.title;
  $('#purchase-dialog-rule').textContent = selection.rule;
  $('#purchase-dialog-plan').textContent = selection.plan;
  $('#purchase-dialog-price').textContent = selection.price;
  $('#purchase-form [name=ruleId]').value = selection.body.ruleId;
  $('#purchase-form [name=priceVersionId]').value = selection.body.priceVersionId;
  $('#purchase-dialog-status').textContent = '';
  $('#purchase-confirm').disabled = false;
  $('#purchase-dialog-cancel').disabled = false;
  $('#purchase-dialog').showModal();
}

function renderPlanFilters() {
  const root = $('#plan-line-filters');
  root.replaceChildren();
  const offerings = [];
  const seen = new Set();
  for (const plan of purchasePlans) {
    if (!plan.offering || seen.has(plan.offering.id)) continue;
    seen.add(plan.offering.id);
    offerings.push(plan.offering);
  }
  if (selectedOffering && !seen.has(selectedOffering)) selectedOffering = '';
  for (const offering of [{ id: '', name: '全部线路' }, ...offerings]) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'plan-filter-chip';
    button.textContent = offering.name;
    button.setAttribute('aria-pressed', String(selectedOffering === offering.id));
    button.addEventListener('click', () => { selectedOffering = offering.id; renderPlanFilters(); renderPurchaseCards(); });
    root.append(button);
  }
}

function appendPlanFact(root, label, value) {
  const group = document.createElement('div');
  const term = document.createElement('dt'); term.textContent = label;
  const fact = document.createElement('dd'); fact.textContent = value;
  group.append(term, fact); root.append(group);
}

function renderPurchaseCards() {
  const root = $('#plan-card-grid');
  root.replaceChildren();
  const plans = filterPurchasePlans(purchasePlans, selectedOffering);
  if (!plans.length) {
    const empty = document.createElement('div');
    empty.className = 'plan-catalog-empty';
    empty.textContent = purchasePlans.length ? '当前线路筛选下暂无套餐。' : '所选规则的入口节点暂无已发布套餐，请联系平台配置对应线路商品。';
    root.append(empty);
    return;
  }
  for (const plan of plans) {
    const view = planPurchaseCard(plan);
    const card = document.createElement('article'); card.className = 'plan-purchase-card';
    const eyebrow = document.createElement('span'); eyebrow.className = 'plan-card-eyebrow'; eyebrow.textContent = view.offeringName;
    const title = document.createElement('h3'); title.textContent = view.name;
    const route = document.createElement('p'); route.className = 'plan-card-route'; route.textContent = view.route;
    const facts = document.createElement('dl');
    appendPlanFact(facts, '独立流量', view.quota);
    appendPlanFact(facts, '带宽上限', view.bandwidth);
    appendPlanFact(facts, '有效周期', view.period);
    appendPlanFact(facts, '资源模式', view.resourceMode);
    const footer = document.createElement('div'); footer.className = 'plan-card-footer';
    const price = document.createElement('div'); price.className = 'plan-card-price';
    const amount = document.createElement('strong'); amount.textContent = view.price;
    const suffix = document.createElement('small'); suffix.textContent = ` / ${view.period}`;
    price.append(amount, suffix);
		const configure = document.createElement('button'); configure.type = 'button'; configure.textContent = '购买套餐'; configure.disabled = !canPurchase;
    configure.addEventListener('click', () => openPurchaseDialog(plan));
    footer.append(price, configure);
    card.append(eyebrow, title, route, facts, footer); root.append(card);
  }
}

function ensurePlanCatalogLoader(api) {
  if (planCatalogLoader && planCatalogAPI === api) return;
  planCatalogAPI = api;
  planCatalogLoader = createLatestPlanLoader({
    load: () => api('/api/plans'),
    onLoading: () => {
      catalogRuleId = '';
      canPurchase = false;
      purchasePlans = [];
      selectedOffering = '';
      renderPlanFilters(); renderPurchaseCards();
      $('#plan-availability').textContent = '套餐购买不关联规则；创建规则时再选择已购买套餐。';
    },
    onSuccess: data => {
      purchasePlans = Array.isArray(data.items) ? data.items : [];
      canPurchase = canManagePurchase && purchasePlans.length > 0;
      renderPlanFilters(); renderPurchaseCards();
      $('#plan-availability').textContent = '以下套餐购买后可被多条规则关联；每条规则单独计量，共享套餐总额度。';
    },
    onError: error => {
      catalogRuleId = '';
      canPurchase = false;
      purchasePlans = [];
      renderPlanFilters(); renderPurchaseCards();
      $('#plan-availability').textContent = '套餐加载失败，请稍后重试。';
      $('#business-message').textContent = error.message;
    },
  });
}

async function loadCompatiblePlans(api) {
  ensurePlanCatalogLoader(api);
  return planCatalogLoader('');
}

export function bindBusinessTools(api, refresh, navigate = navigateWorkspace) {
  navigateWorkspace = navigate;
  ensurePlanCatalogLoader(api);
  tronStatusPoller?.stop();
  tronStatusPoller = createTronStatusPoller({
    load: orderID => api(`/api/payments/tron/orders/${orderID}`),
    onOrder: async order => {
      renderTronInvoice(order);
      if (order.state === 'credited') await refresh();
    },
    onError: error => {
      if (activeTronOrder) $('#tron-payment-message').textContent = `自动刷新失败：${error.message}`;
    },
  });
  externalCheckoutSession = createExternalCheckoutSession({
    createKey: () => crypto.randomUUID(),
    submit: (body, key) => api('/api/payments/plan-checkout', { method: 'POST', body, key }),
    tenantId: '',
  });
  purchaseSession = createPurchaseSession({
    createKey: () => crypto.randomUUID(),
    submit: (body, key) => api('/api/subscriptions/purchase', { method: 'POST', body, key }),
    afterSubmit: refresh,
    onBusy: busy => {
      $('#purchase-confirm').disabled = busy || !canPurchase;
      $('#purchase-dialog-cancel').disabled = busy;
      $('#checkout-balance-pay').disabled = busy;
      $('#checkout-external-pay').disabled = busy;
      $('#checkout-tron-pay').disabled = busy;
      if (busy) $('#checkout-status').textContent = '正在使用钱包余额支付…';
    },
    onSuccess: (_result, selection) => {
      externalCheckoutSession.clear(selection);
      $('#business-message').textContent = selection.body.ruleId ? '本规则套餐已购买并开始计时；节点未部署时配置会继续等待交付。' : '套餐已购买并开始计时，请到转发规则创建规则并绑定入口节点。';
      $('#checkout-status').textContent = '';
      renderPaymentCheckout();
      navigateWorkspace('billing');
    },
    onError: error => { $('#checkout-status').textContent = error.message; renderPaymentCheckout(); },
  });
  $('#top-up-form').addEventListener('submit', async event => {
    event.preventDefault(); const button = event.target.querySelector('button'); button.disabled = true;
    let method;
    let tronRequest;
    try {
      const amountCents = usdInputToCents(new FormData(event.target).get('amountUSD'));
      if (topUp?.amountCents !== amountCents) topUp = { amountCents, key: crypto.randomUUID() };
      method = new FormData(event.target).get('method');
      const path = method === 'tron' ? '/api/payments/tron/orders' : '/api/payments/checkout';
      tronRequest = method === 'tron' ? beginTronRequest() : undefined;
      const order = await api(path, { method: 'POST', body: { amountCents }, key: topUp.key });
      if (method === 'tron') {
        if (!currentTronRequest(tronRequest)) return;
        showTronInvoice(order);
        return;
      }
      if (order.checkoutUrl) {
        const url = new URL(order.checkoutUrl);
        if (url.protocol !== 'https:' || url.hostname !== 'checkout.stripe.com') throw new Error('支付跳转地址无效');
        location.assign(url.href);
      } else { setBusinessStatus('finance', '订单状态已更新，请刷新查看钱包和账本。'); await refresh(); }
    } catch (error) {
      if (method !== 'tron' || currentTronRequest(tronRequest)) setBusinessStatus('finance', error.message);
    } finally {
      if (method !== 'tron' || currentTronRequest(tronRequest)) button.disabled = false;
    }
  });
  $('#wallet-ledger-refresh').addEventListener('click', async () => {
    try {
      const rows = await api('/api/billing/ledger'); const root = $('#wallet-ledger-rows'); root.replaceChildren();
      for (const entry of rows) { const row = root.insertRow(); for (const value of [new Date(entry.createdAt).toLocaleString(), entry.kind, entry.ruleId || '钱包', signedUsd(entry.amountCents), entry.reason]) row.insertCell().textContent = value; }
    } catch (error) { setBusinessStatus('finance', error.message); }
  });
  $('#purchase-form').addEventListener('submit', async event => {
    event.preventDefault();
    suppressPurchaseDialogClose = true;
    $('#purchase-dialog').close();
    const refreshed = await preparePaymentCheckout(refresh, error => {
      $('#checkout-status').textContent = `支付状态刷新失败：${error.message}`;
    });
    if (refreshed) $('#checkout-status').textContent = '';
    renderPaymentCheckout();
    navigateWorkspace('checkout');
  });
  $('#checkout-back').addEventListener('click', () => {
    if (externalCheckoutSession.isSubmitting()) return;
    if (!purchaseSession.close()) return;
    renderPaymentCheckout();
    navigateWorkspace('billing');
  });
  $('#checkout-balance-pay').addEventListener('click', async () => {
    await purchaseSession.submit();
  });
  $('#checkout-external-pay').addEventListener('click', async () => {
    const selection = purchaseSession.current();
    const view = paymentCheckoutPresentation(selection, walletState, paymentState);
    if (!selection || !view.externalEnabled) return;
    const button = $('#checkout-external-pay'); button.disabled = true;
    $('#checkout-back').disabled = true;
    $('#checkout-balance-pay').disabled = true;
    $('#checkout-status').textContent = '正在创建外部支付订单…';
    try {
      const order = await externalCheckoutSession.submit(selection);
      if (order?.stale) return;
      if (!order.checkoutUrl) {
        $('#checkout-status').textContent = planPaymentOrderPresentation(order).message;
        await refresh();
        renderPaymentCheckout();
        return;
      }
      const confirmation = stripeCheckoutConfirmation(view, order);
      if (confirmation.required) {
        $('#checkout-status').textContent = confirmation.message;
        if (!window.confirm(confirmation.message)) {
          renderPaymentCheckout();
          return;
        }
      }
      const url = new URL(order.checkoutUrl);
      if (url.protocol !== 'https:' || url.hostname !== 'checkout.stripe.com') throw new Error('支付跳转地址无效');
      location.assign(url.href);
    } catch (error) {
      $('#checkout-status').textContent = error.message;
      renderPaymentCheckout();
    }
  });
  $('#checkout-tron-pay').addEventListener('click', async () => {
    const selection = purchaseSession.current();
    if (!selection || !paymentState.tron || walletState.spendingFrozen) return;
    const serialized = JSON.stringify(selection.body);
    if (tronCheckout?.serialized !== serialized) tronCheckout = { serialized, key: crypto.randomUUID() };
    const request = beginTronRequest();
    $('#checkout-tron-pay').disabled = true;
    $('#checkout-status').textContent = '正在创建 TRON USDT 收款订单…';
    try {
      const order = await api('/api/payments/tron/orders', { method: 'POST', body: selection.body, key: tronCheckout.key });
      if (!currentTronRequest(request)) return;
      showTronInvoice(order);
      $('#checkout-status').textContent = '';
    } catch (error) {
      if (currentTronRequest(request)) $('#checkout-status').textContent = error.message;
    } finally { if (currentTronRequest(request)) renderPaymentCheckout(); }
  });
  $('#tron-claim-form').addEventListener('submit', async event => {
    event.preventDefault();
    if (!activeTronOrder?.id) return;
    const request = beginTronRequest();
    const button = $('#tron-claim-submit'); button.disabled = true;
    try {
      const txHash = String(new FormData(event.target).get('txHash') || '');
      const order = await api(`/api/payments/tron/orders/${activeTronOrder.id}/claim`, { method: 'POST', body: { txHash } });
      if (!currentTronRequest(request)) return;
      showTronInvoice(order);
    } catch (error) { if (currentTronRequest(request)) $('#tron-payment-message').textContent = error.message; }
    finally { if (currentTronRequest(request)) button.disabled = false; }
  });
  $('#tron-payment-refresh').addEventListener('click', async () => {
    if (!activeTronOrder?.id) return;
    const request = beginTronRequest();
    try {
      const order = await api(`/api/payments/tron/orders/${activeTronOrder.id}`);
      if (!currentTronRequest(request)) return;
      showTronInvoice(order);
      if (order.state === 'credited') await refresh();
    } catch (error) { if (currentTronRequest(request)) $('#tron-payment-message').textContent = error.message; }
  });
  $('#tron-payment-copy-address').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(activeTronOrder?.collectionAddress || '');
      $('#tron-payment-message').textContent = '收款地址已复制。';
    } catch { $('#tron-payment-message').textContent = '无法自动复制，请手动选择收款地址。'; }
  });
  $('#tron-payment-close').addEventListener('click', () => invalidateTronRequests(true));
  $('#tron-payment-close-icon').addEventListener('click', () => invalidateTronRequests(true));
  $('#tron-payment-dialog').addEventListener('close', () => {
    if (activeTronOrder) invalidateTronRequests();
    activeTronOrder = undefined;
  });
  $('#purchase-dialog-cancel').addEventListener('click', () => {
    if (purchaseSession.close()) $('#purchase-dialog').close();
  });
  $('#purchase-dialog').addEventListener('cancel', event => {
    if (!purchaseSession.close()) event.preventDefault();
  });
  $('#purchase-dialog').addEventListener('close', () => {
    if (suppressPurchaseDialogClose) {
      suppressPurchaseDialogClose = false;
      return;
    }
    purchaseSession.close();
    if (!purchaseSession.isSubmitting()) $('#purchase-dialog-status').textContent = '';
  });
  $('#subscription-refresh').addEventListener('click', () => refreshSubscriptions(api));
}

export function autoRenewMessage(period) {
  return ({ 'insufficient-balance': '余额不足，自动续费未完成', 'wallet-frozen': '钱包已冻结，自动续费未完成', 'unavailable': '套餐或节点暂停销售，未续费', 'tenant-inactive': '企业已停用，未续费' })[period.autoRenewError] || (period.autoRenew ? '流量耗尽时从余额续费' : '未开启');
}

async function refreshSubscriptions(api) {
  const request = businessRefreshGate.current();
  if (!request) return;
  try {
    const periods = await api('/api/subscriptions?all=true');
    if (!businessRefreshGate.isCurrent(request)) return;
    const root = $('#subscription-rows'); root.replaceChildren();
    if (!periods.length) { const cell = root.insertRow().insertCell(); cell.colSpan = 6; cell.className = 'table-empty'; cell.textContent = '暂无已购买套餐'; }
    for (const period of periods) {
      const row = root.insertRow();
      for (const value of [period.plan.name, `${(period.usedBytes / 1e9).toLocaleString()} / ${(period.plan.quotaBytes / 1e9).toLocaleString()} GB`, `${period.plan.bandwidthMbps} Mbps`, new Date(period.endsAt).toLocaleString(), ({ active: '有效', expired: '已到期', 'quota-exhausted': '流量已用完', closed: '历史周期' })[period.status] || period.status]) row.insertCell().textContent = value;
      const cell = row.insertCell();
      const detail = document.createElement('small'); detail.textContent = autoRenewMessage(period); cell.append(detail);
      if (canManagePurchase && !period.closed && new Date(period.endsAt) > new Date()) {
        const button = document.createElement('button'); button.type = 'button'; button.className = 'secondary'; button.textContent = period.autoRenew ? '关闭自动续费' : '开启自动续费';
        button.addEventListener('click', async () => {
          if (!period.autoRenew && !window.confirm(`开启后，每次流量耗尽将从钱包扣除 $${(period.plan.priceCents / 100).toFixed(2)}，按当前套餐续购 ${(period.plan.periodSeconds / 86400).toLocaleString()} 天。余额不足将停止转发。确认开启？`)) return;
          button.disabled = true;
          try {
            await api('/api/subscriptions/auto-renew', { method: 'POST', body: { subscriptionId: period.id, enabled: !period.autoRenew } });
            if (businessRefreshGate.isCurrent(request)) await refreshSubscriptions(api);
          } catch (error) { if (businessRefreshGate.isCurrent(request)) { detail.textContent = error.message; button.disabled = false; } }
        });
        cell.append(button);
      }
    }
  } catch (error) { if (businessRefreshGate.isCurrent(request)) $('#business-message').textContent = error.message; }
}

export async function refreshBusinessTools(api, identity, rules) {
  const refreshRequest = businessRefreshGate.begin(identity.tenantId);
  const [wallet, payment] = await Promise.all([api('/api/billing/wallet'), api('/api/payments/status')]);
  if (!businessRefreshGate.isCurrent(refreshRequest)) return { stale: true };
  walletState = wallet;
  paymentState = payment;
  if (tronRequestGate.setTenant(identity.tenantId)) {
		activeTronOrder = undefined;
		const dialog = $('#tron-payment-dialog');
		if (dialog?.open) dialog.close();
  }
  externalCheckoutSession?.setTenant(identity.tenantId);
  const currentSelection = purchaseSession?.current();
  if (currentSelection && currentSelection.body.ruleId && !rules.some(rule => rule.id === currentSelection.body.ruleId)) {
    purchaseSession.reset();
    externalCheckoutSession.invalidate();
  }
  $('#wallet-status').textContent = wallet.spendingFrozen ? `钱包支出已冻结，请联系平台对账。余额：$${(wallet.balanceCents / 100).toFixed(2)}` : wallet.initialized ? `钱包余额：$${(wallet.balanceCents / 100).toFixed(2)}` : '钱包尚无到账或调整记录';
  const stripeEnabled = payment.stripe === true || (payment.stripe === undefined && payment.enabled === true);
  const tronEnabled = payment.tron === true;
  const topUpMethod = $('#top-up-method');
  for (const item of topUpMethod.options) {
    const enabled = item.value === 'tron' ? tronEnabled : stripeEnabled;
    item.disabled = !enabled;
    item.hidden = !enabled;
  }
  if (tronEnabled) topUpMethod.value = 'tron'; else if (stripeEnabled) topUpMethod.value = 'stripe';
  $('#top-up-form').hidden = (!stripeEnabled && !tronEnabled) || !['owner', 'admin'].includes(identity.role) || wallet.spendingFrozen;
  $('#payment-availability').textContent = payment.enabled ? 'Stripe 以服务端验签回调为准；TRON USDT 仅在固化交易严格匹配后入账。返回页面或提交哈希都不代表充值成功。' : '支付渠道尚未启用。';
  purchaseRules = rules;
  let pendingSelection = !purchaseSession.current() ? externalCheckoutSession.pendingSelection() : undefined;
  canManagePurchase = canOpenPackageCheckout(identity, rules, wallet);
  await refreshSubscriptions(api);
  if (!businessRefreshGate.isCurrent(refreshRequest)) return { stale: true };
  await loadCompatiblePlans(api);
  if (!businessRefreshGate.isCurrent(refreshRequest)) return { stale: true };
  let reconciled;
  if (globalThis.location?.hash === '#/checkout') {
    try {
      reconciled = await externalCheckoutSession.reconcile(
        id => api(`/api/payments/orders/${encodeURIComponent(id)}`),
        () => api('/api/payments/orders?purpose=plan-purchase'),
      );
      if (reconciled?.stale || !businessRefreshGate.isCurrent(refreshRequest)) return { stale: true };
      pendingSelection = reconciled?.selection || pendingSelection;
      if (!pendingSelection && reconciled?.order) {
        const rule = rules.find(item => item.id === reconciled.order.ruleId);
        const plan = purchasePlans.find(item => item.id === reconciled.order.priceVersionId);
        if (plan && (!reconciled.order.ruleId || rule)) pendingSelection = purchaseDialogSelection(plan, rule);
      }
    } catch (error) {
      if (businessRefreshGate.isCurrent(refreshRequest)) $('#checkout-status').textContent = `支付订单状态读取失败：${error.message}`;
    }
  }
  if (!purchaseSession.current() && pendingSelection && globalThis.location?.hash === '#/checkout') {
    const rule = rules.find(item => item.id === pendingSelection.body.ruleId);
    const plan = purchasePlans.find(item => item.id === pendingSelection.body.priceVersionId);
    if (plan && (!pendingSelection.body.ruleId || rule)) purchaseSession.open(purchaseDialogSelection(plan, rule));
  }
  renderPaymentCheckout();
  if (reconciled?.order) {
    const orderView = planPaymentOrderPresentation(reconciled.order);
    $('#checkout-status').textContent = orderView.message;
    if (orderView.complete) {
      purchaseSession.reset();
      $('#business-message').textContent = orderView.message;
      renderPaymentCheckout();
      navigateWorkspace('billing');
    }
  }
  return { stale: false };
}
