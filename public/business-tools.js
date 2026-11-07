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
let paymentState = { enabled: false };
let planCatalogAPI;
let planCatalogLoader;
let purchaseSession;
let externalCheckoutSession;
let navigateWorkspace = view => { location.hash = `#/${view}`; };
let suppressPurchaseDialogClose = false;
const businessRefreshGate = createTenantRefreshGate();

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

export function paymentCheckoutPresentation(selection, wallet, payment) {
  const priceCents = Math.max(0, Number(selection?.priceCents) || 0);
  const balanceCents = Math.max(0, Number(wallet?.balanceCents) || 0);
  const frozen = wallet?.spendingFrozen === true;
  const enough = balanceCents >= priceCents;
  return {
    amount: usd(priceCents),
    balance: usd(balanceCents),
    balanceAfter: usd(enough ? balanceCents - priceCents : 0),
    balanceEnabled: !frozen && priceCents > 0 && enough,
    balanceDetail: frozen
      ? '钱包支出已冻结，请联系平台完成对账。'
      : enough
        ? `支付后钱包余额 ${usd(balanceCents - priceCents)}`
        : `余额不足，还差 ${usd(priceCents - balanceCents)}`,
    externalEnabled: !frozen && priceCents > 0 && payment?.enabled === true,
    externalDetail: frozen
      ? '钱包支出已冻结，不能发起新的支付。'
      : payment?.enabled
        ? '前往 Stripe 完成充值；只有验签回调到账后才能使用余额购买套餐。'
        : '尚未启用其他支付方式。',
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
        const result = await submit({ amountCents: selection.priceCents }, entry.key);
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
  $('#checkout-balance-detail').textContent = view.balanceDetail;
  $('#checkout-external-detail').textContent = view.externalDetail;
  $('#checkout-back').disabled = busy;
  $('#checkout-balance-pay').disabled = !view.balanceEnabled || busy;
  $('#checkout-external-pay').disabled = !view.externalEnabled || busy;
}

export function businessViewChanged(view) {
  if (view !== 'checkout' && externalCheckoutSession?.isSubmitting()) externalCheckoutSession.invalidate();
}

export function resetBusinessTools() {
  businessRefreshGate.reset();
  planCatalogLoader?.invalidate?.();
  topUp = undefined;
  walletState = { initialized: false, balanceCents: 0, spendingFrozen: false };
  paymentState = { enabled: false };
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
  externalCheckoutSession = createExternalCheckoutSession({
    createKey: () => crypto.randomUUID(),
    submit: (body, key) => api('/api/payments/checkout', { method: 'POST', body, key }),
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
    try {
      const amountCents = Number(new FormData(event.target).get('amountCents'));
      if (topUp?.amountCents !== amountCents) topUp = { amountCents, key: crypto.randomUUID() };
      const order = await api('/api/payments/checkout', { method: 'POST', body: { amountCents }, key: topUp.key });
      if (order.checkoutUrl) {
        const url = new URL(order.checkoutUrl);
        if (url.protocol !== 'https:' || url.hostname !== 'checkout.stripe.com') throw new Error('支付跳转地址无效');
        location.assign(url.href);
      } else { setBusinessStatus('finance', '订单状态已更新，请刷新查看钱包和账本。'); await refresh(); }
    } catch (error) { setBusinessStatus('finance', error.message); }
    finally { button.disabled = false; }
  });
  $('#wallet-ledger-refresh').addEventListener('click', async () => {
    try {
      const rows = await api('/api/billing/ledger'); const root = $('#wallet-ledger-rows'); root.replaceChildren();
      for (const entry of rows) { const row = root.insertRow(); for (const value of [new Date(entry.createdAt).toLocaleString(), entry.kind, entry.ruleId || '钱包', `${entry.amountCents > 0 ? '+' : ''}${(entry.amountCents / 100).toFixed(2)}`, entry.reason]) row.insertCell().textContent = value; }
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
        $('#checkout-status').textContent = '支付订单状态已更新。请刷新钱包，到账后使用余额完成套餐购买。';
        await refresh();
        renderPaymentCheckout();
        return;
      }
      const url = new URL(order.checkoutUrl);
      if (url.protocol !== 'https:' || url.hostname !== 'checkout.stripe.com') throw new Error('支付跳转地址无效');
      location.assign(url.href);
    } catch (error) {
      $('#checkout-status').textContent = error.message;
      renderPaymentCheckout();
    }
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
  externalCheckoutSession?.setTenant(identity.tenantId);
  const currentSelection = purchaseSession?.current();
  if (currentSelection && currentSelection.body.ruleId && !rules.some(rule => rule.id === currentSelection.body.ruleId)) {
    purchaseSession.reset();
    externalCheckoutSession.invalidate();
  }
  $('#wallet-status').textContent = wallet.spendingFrozen ? `钱包支出已冻结，请联系平台对账。余额：$${(wallet.balanceCents / 100).toFixed(2)}` : wallet.initialized ? `钱包余额：$${(wallet.balanceCents / 100).toFixed(2)}` : '钱包尚无到账或调整记录';
  $('#top-up-form').hidden = !payment.enabled || !['owner', 'admin'].includes(identity.role) || wallet.spendingFrozen;
  $('#payment-availability').textContent = payment.enabled ? '支付结果以验签回调到账为准，返回页面不代表充值成功。' : '支付渠道尚未启用。';
  purchaseRules = rules;
  const pendingSelection = !purchaseSession.current() ? externalCheckoutSession.pendingSelection() : undefined;
  canManagePurchase = canOpenPackageCheckout(identity, rules, wallet);
  await refreshSubscriptions(api);
  if (!businessRefreshGate.isCurrent(refreshRequest)) return { stale: true };
  await loadCompatiblePlans(api);
  if (!businessRefreshGate.isCurrent(refreshRequest)) return { stale: true };
  if (!purchaseSession.current() && pendingSelection && globalThis.location?.hash === '#/checkout') {
    const rule = rules.find(item => item.id === pendingSelection.body.ruleId);
    const plan = purchasePlans.find(item => item.id === pendingSelection.body.priceVersionId);
    if (plan && (!pendingSelection.body.ruleId || rule)) purchaseSession.open(purchaseDialogSelection(plan, rule));
  }
  renderPaymentCheckout();
  return { stale: false };
}
