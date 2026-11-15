import test from 'node:test';
import assert from 'node:assert/strict';

import { assessConnection, buildInquirySummary } from '../js/scope-checker.js';

test('accepts a specified public domain over TCP', () => {
  const result = assessConnection({
    direction: 'cn-to-global',
    targetType: 'domain',
    target: 'api.example.com',
    protocol: 'TCP',
    port: '443',
  });

  assert.equal(result.inScope, true);
  assert.match(result.summary, /在当前服务范围内/);
});

test('accepts a specified public IPv4 address over UDP', () => {
  const result = assessConnection({
    direction: 'global-to-cn',
    targetType: 'ip',
    target: '203.0.113.24',
    protocol: 'UDP',
    port: '6000',
  });

  assert.equal(result.inScope, true);
  assert.match(result.detail, /公网目标/);
});

test('rejects private and loopback IP targets', () => {
  for (const target of ['10.0.0.8', '172.16.4.5', '192.168.1.10', '127.0.0.1']) {
    const result = assessConnection({
      direction: 'cn-to-global',
      targetType: 'ip',
      target,
      protocol: 'TCP',
      port: '443',
    });

    assert.equal(result.inScope, false, target);
    assert.equal(result.reason, 'private-target');
    assert.doesNotMatch(result.detail, /可提供内网|通用上网|全局代理/);
  }
});

test('rejects internal address types and local domains', () => {
  const internal = assessConnection({
    direction: 'global-to-cn',
    targetType: 'internal',
    target: 'erp.intranet',
    protocol: 'TCP',
    port: '443',
  });
  const local = assessConnection({
    direction: 'global-to-cn',
    targetType: 'domain',
    target: 'erp.local',
    protocol: 'TCP',
    port: '443',
  });

  assert.equal(internal.reason, 'private-target');
  assert.equal(local.reason, 'private-target');
});

test('rejects unsupported protocols and invalid ports', () => {
  const protocol = assessConnection({
    direction: 'cn-to-global',
    targetType: 'domain',
    target: 'api.example.com',
    protocol: 'HTTP',
    port: '443',
  });
  const port = assessConnection({
    direction: 'cn-to-global',
    targetType: 'domain',
    target: 'api.example.com',
    protocol: 'TCP',
    port: '0',
  });

  assert.equal(protocol.reason, 'unsupported-protocol');
  assert.equal(port.reason, 'invalid-port');
});

test('builds a deterministic inquiry summary for an accepted request', () => {
  const summary = buildInquirySummary({
    direction: 'cn-to-global',
    targetType: 'domain',
    target: 'api.example.com',
    protocol: 'TCP',
    port: '443',
  });

  assert.equal(summary, [
    'RelaDock 连接需求',
    '连接方向：中国大陆 → 海外',
    '目标类型：公网域名',
    '目标：api.example.com',
    '协议与端口：TCP/443',
    '待补充：业务场景、预计月流量、峰值速率、期望交付时间',
  ].join('\n'));
});

test('does not build an inquiry summary for an out-of-scope request', () => {
  assert.equal(
    buildInquirySummary({
      direction: 'cn-to-global',
      targetType: 'internal',
      target: '10.0.0.8',
      protocol: 'TCP',
      port: '443',
    }),
    '',
  );
});
