import { resourceModeLabel } from './resource-mode.js';

const offeringStates = { draft: '草稿', published: '销售中', paused: '暂停销售', 'sold-out': '售罄', retired: '已下架' };

export function offeringStateLabel(state) {
  return offeringStates[state] || state;
}

export function priceableOfferings(items = []) {
  return items.filter(item => item.state !== 'retired');
}

export function offeringRouteLabel(offering) {
  return `${offering.sourceMarket || '来源未填写'} → ${offering.destinationMarket || '目标未填写'}`;
}

export function offeringActionDescriptor(kind, offering) {
  if (kind === 'delete') {
    if (offering.state !== 'draft' || !offering.deletable) return null;
    return {
      title: `删除草稿线路 · ${offering.name}`,
      description: '此草稿尚未关联任何套餐价格版本。删除后不可恢复，平台审计仍会保留。',
      confirmLabel: '确认删除草稿',
      danger: true,
      path: '/api/admin/offerings/delete',
      body: { offeringId: offering.id, expectedRevision: offering.revision },
      successText: '草稿线路已删除。',
    };
  }
  const definitions = {
    published: { title: '发布销售', confirmLabel: '确认发布', description: '对应入口节点未部署时也可发布并接受付款，套餐从付款成功起立即计时；规则会等待节点交付。发布不代表线路端到端健康。', successText: '线路商品已发布销售。' },
    paused: { title: '暂停销售', confirmLabel: '确认暂停销售', description: '暂停后不会再出现在客户可购目录；已购买套餐和已有转发不会自动停止。', successText: '线路商品已暂停销售。' },
    'sold-out': { title: '设为售罄', confirmLabel: '确认设为售罄', description: '售罄后不会再出现在客户可购目录；已购买套餐和已有转发不会自动停止。', successText: '线路商品已设为售罄。' },
    retired: { title: '下架线路', confirmLabel: '确认下架', description: '下架是终态，不能重新发布；历史套餐、购买记录和平台审计仍会保留。', successText: '线路商品已下架。' },
  };
  const definition = definitions[kind];
  if (!definition) return null;
  return {
    ...definition,
    title: `${definition.title} · ${offering.name}`,
    danger: kind === 'retired',
    path: '/api/admin/offerings/state',
    body: { offeringId: offering.id, expectedRevision: offering.revision, state: kind },
  };
}

export function createOfferingMutationRunner({ mutate, refresh, committedText, onRefreshFailure = () => {} }) {
  let didCommit = false;
  const run = async () => {
    if (!didCommit) {
      await mutate();
      didCommit = true;
    }
    try {
      await refresh();
    } catch (error) {
      onRefreshFailure(error);
      throw new Error(`${committedText}，但列表刷新失败：${error.message || '操作失败'}。再次确认只会重试刷新。`);
    }
  };
  run.committed = () => didCommit;
  return run;
}

export function planInputFromFields(fields) {
  return {
    offeringId: fields.offeringId,
    code: fields.code,
    name: fields.name,
    priceCents: Math.round(Number(fields.priceUSD) * 100),
    quotaBytes: Number(fields.quotaGB) * 1e9,
    bandwidthMbps: Number(fields.bandwidthMbps),
    periodSeconds: Number(fields.periodDays) * 86400,
  };
}

export function adminPlanOfferingLabel(plan) {
  if (!plan.offering) return '历史版本 · 未绑定线路 · 不可新购';
  const offering = plan.offering;
  return `${offering.name} · ${offering.nodeId} · ${offeringRouteLabel(offering)}`;
}

export function planCatalogLabel(plan) {
  const offering = plan.offering;
  const route = offering ? `${offering.name} · ${offeringRouteLabel(offering)}` : '未绑定线路';
  return `${route} · ${plan.name} · ${resourceModeLabel(plan.resourceMode)} · ${(plan.quotaBytes / 1e9).toLocaleString()} GB · ${plan.bandwidthMbps} Mbps · ${(plan.periodSeconds / 86400).toLocaleString()} 天 · $${(plan.priceCents / 100).toFixed(2)}`;
}
