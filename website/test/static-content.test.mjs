import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const root = new URL('../', import.meta.url);

async function read(relativePath) {
  return readFile(new URL(relativePath, root), 'utf8');
}

test('page states the product and customer boundary plainly', async () => {
  const html = await read('index.html');

  for (const required of [
    '只连接指定应用',
    '跨境电商企业',
    'ERP / SaaS 服务商',
    '指定公网 IP 或域名',
    '不接企业内网',
    '每条规则独立',
  ]) {
    assert.match(html, new RegExp(required.replace('/', '\\/')));
  }
});

test('page excludes legacy fabricated claims and fake form success', async () => {
  const [html, script] = await Promise.all([read('index.html'), read('js/main.js')]);
  const source = `${html}\n${script}`;

  for (const prohibited of [
    '99.99%',
    '3000+',
    '40+ 节点',
    '30ms',
    '10Gbps',
    '5 分钟响应',
    '提交成功',
    '自动故障切换',
  ]) {
    assert.doesNotMatch(source, new RegExp(prohibited.replace('+', '\\+')));
  }
});

test('page contains real destinations and four product UI fragments', async () => {
  const html = await read('index.html');

  assert.match(html, /https:\/\/console\.reladock\.com/);
  assert.doesNotMatch(html, /mailto:/);
  assert.doesNotMatch(html, /sales@reladock\.com/);
  assert.equal((html.match(/data-ui/g) || []).length, 4);
});

test('visual system avoids gradients and rest-state shadows', async () => {
  const css = await read('css/style.css');

  assert.doesNotMatch(css, /gradient\(/i);
  assert.doesNotMatch(css, /box-shadow\s*:/i);
  assert.match(css, /prefers-reduced-motion/);
  assert.match(css, /:focus-visible/);
  assert.match(css, /\.scope-result-actions \[hidden\]\s*\{[^}]*display:\s*none\s*!important/s);
});

test('website uses the shared blue RelaDock brand mark', async () => {
  const [html, mark, fullLogo] = await Promise.all([
    read('index.html'),
    read('reladock-mark.svg'),
    read('reladock-logo.svg'),
  ]);

  assert.match(html, /<img class="brand-mark" src="reladock-mark\.svg"/);
  assert.match(html, /<link rel="icon" href="reladock-mark\.svg"/);
  for (const source of [mark, fullLogo]) {
    assert.match(source, /#2563EB/);
    assert.match(source, /#60A5FA/);
    assert.match(source, /#081A33/);
    assert.doesNotMatch(source, /#DAFA9C/i);
  }
});
