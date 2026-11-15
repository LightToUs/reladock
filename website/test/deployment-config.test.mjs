import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const nginxConfig = await readFile(
  new URL('../../deploy/reladock-nginx.conf', import.meta.url),
  'utf8',
);

test('console upstream explicitly negotiates the control server TLS version', () => {
  assert.match(nginxConfig, /proxy_ssl_protocols\s+TLSv1\.3;/);
  assert.match(nginxConfig, /proxy_ssl_name\s+console\.reladock\.com;/);
  assert.match(nginxConfig, /proxy_ssl_verify\s+on;/);
  assert.match(nginxConfig, /proxy_ssl_verify_depth\s+3;/);
});

test('public-site proxy configuration does not claim the agent listener', () => {
  assert.doesNotMatch(nginxConfig, /9443/);
});
