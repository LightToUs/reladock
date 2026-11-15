const DIRECTION_LABELS = Object.freeze({
  'cn-to-global': '中国大陆 → 海外',
  'global-to-cn': '海外 → 中国大陆',
});

const TARGET_LABELS = Object.freeze({
  domain: '公网域名',
  ip: '公网 IP',
});

function isPrivateIPv4(value) {
  const octets = value.split('.').map(Number);
  if (octets.length !== 4 || octets.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) {
    return false;
  }

  const [first, second] = octets;
  return (
    first === 10 ||
    first === 127 ||
    first === 0 ||
    (first === 169 && second === 254) ||
    (first === 172 && second >= 16 && second <= 31) ||
    (first === 192 && second === 168)
  );
}

function isLocalDomain(value) {
  const normalized = value.toLowerCase();
  return (
    normalized === 'localhost' ||
    normalized.endsWith('.localhost') ||
    normalized.endsWith('.local') ||
    normalized.endsWith('.internal') ||
    normalized.endsWith('.intranet')
  );
}

function rejected(reason, summary, detail) {
  return { inScope: false, reason, summary, detail };
}

export function assessConnection(input) {
  const protocol = String(input.protocol || '').toUpperCase();
  const port = Number(input.port);
  const targetType = String(input.targetType || '');
  const target = String(input.target || '').trim();

  if (!['TCP', 'UDP'].includes(protocol)) {
    return rejected(
      'unsupported-protocol',
      '需要进一步确认协议',
      '当前按指定 TCP 或 UDP 端口交付，不终结或代管上层代理、VPN 与订阅协议。',
    );
  }

  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return rejected('invalid-port', '请填写有效端口', '端口应为 1–65535 之间的整数，并对应一个明确的业务服务。');
  }

  if (
    targetType === 'internal' ||
    !['domain', 'ip'].includes(targetType) ||
    !target ||
    isLocalDomain(target) ||
    (targetType === 'ip' && isPrivateIPv4(target))
  ) {
    return rejected(
      'private-target',
      '不在当前服务范围内',
      'RelaDock 当前只连接指定公网 IP 或公网域名，不接入企业内网、私网 ERP、站点路由或终端设备。',
    );
  }

  return {
    inScope: true,
    reason: 'accepted',
    summary: '在当前服务范围内',
    detail: `可按 ${protocol}/${port} 连接指定公网目标；实际可达性、容量与交付周期需在开通前测试确认。`,
  };
}

export function buildInquirySummary(input) {
  const assessment = assessConnection(input);
  if (!assessment.inScope) {
    return '';
  }

  const protocol = String(input.protocol).toUpperCase();
  const port = Number(input.port);
  const target = String(input.target).trim();
  const direction = DIRECTION_LABELS[input.direction] || '待确认';
  const targetType = TARGET_LABELS[input.targetType] || '待确认';
  return [
    'RelaDock 连接需求',
    `连接方向：${direction}`,
    `目标类型：${targetType}`,
    `目标：${target}`,
    `协议与端口：${protocol}/${port}`,
    '待补充：业务场景、预计月流量、峰值速率、期望交付时间',
  ].join('\n');
}
