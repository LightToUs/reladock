import assert from 'node:assert/strict';
import test from 'node:test';
import { createGoogleLoginController } from '../public/google-login.js';

function buttonFixture() {
  return {
    hidden: true,
    disabled: false,
    listeners: new Map(),
    addEventListener(type, listener) { this.listeners.set(type, listener); },
  };
}

function controllerFixture(overrides = {}) {
  const button = buttonFixture();
  const linkCancel = buttonFixture();
  const message = { textContent: '' };
  const location = {
    href: 'https://console.example.com/',
    assigned: '',
    assign(value) { this.assigned = value; },
  };
  const history = {
    replaced: '',
    replaceState(_state, _title, value) { this.replaced = value; },
  };
  const availability = [];
  let linkCancels = 0;
  const controller = createGoogleLoginController({
    api: async path => path.endsWith('/status') ? { enabled: false } : {},
    returnTo: '/', button, linkCancel, message, location, history,
    onAvailabilityChange: enabled => availability.push(enabled),
    onLinkCancel: () => { linkCancels++; },
    ...overrides,
  });
  return { controller, button, linkCancel, message, location, history, availability, linkCancels: () => linkCancels };
}

test('disabled status keeps Google login hidden', async () => {
  const fixture = controllerFixture();
  assert.equal(await fixture.controller.load(), false);
  assert.equal(fixture.button.hidden, true);
  assert.deepEqual(fixture.availability, [false]);
});

test('enabled status reveals Google login and starts the fixed enterprise target', async () => {
  const calls = [];
  const fixture = controllerFixture({
    api: async (path, options = {}) => {
      calls.push({ path, options });
      if (path.endsWith('/status')) return { enabled: true };
      return { authorizationURL: 'https://accounts.google.com/o/oauth2/v2/auth?state=opaque' };
    },
  });
  assert.equal(await fixture.controller.load(), true);
  assert.equal(fixture.button.hidden, false);
  await fixture.controller.start();
  assert.deepEqual(calls[1], {
    path: '/api/auth/google/start',
    options: { method: 'POST', body: { returnTo: '/' } },
  });
  assert.equal(fixture.location.assigned, 'https://accounts.google.com/o/oauth2/v2/auth?state=opaque');
  assert.equal(fixture.message.textContent, '');
});

test('admin controller requests only the fixed admin return target', async () => {
  let startOptions;
  const fixture = controllerFixture({
    returnTo: '/admin',
    api: async (path, options = {}) => {
      if (path.endsWith('/status')) return { enabled: true };
      startOptions = options;
      return { authorizationURL: 'https://accounts.google.com/o/oauth2/v2/auth?state=opaque' };
    },
  });
  await fixture.controller.load();
  await fixture.controller.start();
  assert.deepEqual(startOptions.body, { returnTo: '/admin' });
});

test('duplicate starts are suppressed while one request is pending', async () => {
  let starts = 0;
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const fixture = controllerFixture({
    api: async path => {
      if (path.endsWith('/status')) return { enabled: true };
      starts++;
      return pending;
    },
  });
  await fixture.controller.load();
  const first = fixture.controller.start();
  const second = fixture.controller.start();
  assert.equal(starts, 1);
  assert.equal(fixture.button.disabled, true);
  release({ authorizationURL: 'https://accounts.google.com/o/oauth2/v2/auth?state=opaque' });
  await Promise.all([first, second]);
  assert.equal(fixture.location.assigned, 'https://accounts.google.com/o/oauth2/v2/auth?state=opaque');
});

test('invalid authorization URLs and request failures stay on page with a generic message', async () => {
  for (const result of [
    { authorizationURL: 'http://accounts.google.com/o/oauth2/v2/auth?state=opaque' },
    { authorizationURL: 'https://evil.example/o/oauth2/v2/auth?state=opaque' },
    new Error('provider-secret state-secret'),
  ]) {
    const fixture = controllerFixture({
      api: async path => {
        if (path.endsWith('/status')) return { enabled: true };
        if (result instanceof Error) throw result;
        return result;
      },
    });
    await fixture.controller.load();
    await fixture.controller.start();
    assert.equal(fixture.location.assigned, '');
    assert.equal(fixture.message.textContent, 'Google 登录暂时不可用，请稍后重试。');
    assert.doesNotMatch(fixture.message.textContent, /provider-secret|state-secret/);
  }
});

test('callback failure displays a generic message and removes only the marker', () => {
  const fixture = controllerFixture();
  fixture.location.href = 'https://console.example.com/admin?googleAuth=failed&keep=1#/overview';
  assert.equal(fixture.controller.consumeCallbackResult(), 'failed');
  assert.equal(fixture.message.textContent, 'Google 登录未完成，请重试或使用邮箱密码登录。');
  assert.equal(fixture.history.replaced, '/admin?keep=1#/overview');

  fixture.location.href = 'https://console.example.com/admin?keep=1';
  fixture.history.replaced = '';
  assert.equal(fixture.controller.consumeCallbackResult(), '');
  assert.equal(fixture.history.replaced, '');
});

test('first authorization requests existing-account password binding and survives disabled status', async () => {
  const fixture = controllerFixture();
  fixture.location.href = 'https://console.example.com/?googleAuth=link&keep=1#/overview';
  assert.equal(fixture.controller.consumeCallbackResult(), 'link');
  assert.equal(fixture.message.textContent, '首次使用 Google 登录，请输入现有 RelaDock 账号邮箱和密码完成绑定。');
  assert.equal(fixture.linkCancel.hidden, false);
  assert.equal(fixture.history.replaced, '/?keep=1#/overview');
  assert.equal(await fixture.controller.load(), false);
  assert.equal(fixture.message.textContent, '首次使用 Google 登录，请输入现有 RelaDock 账号邮箱和密码完成绑定。');
  fixture.linkCancel.listeners.get('click')({ preventDefault() {} });
  assert.equal(fixture.linkCancels(), 1);
  assert.equal(fixture.linkCancel.hidden, true);
  assert.equal(fixture.message.textContent, '');
});
