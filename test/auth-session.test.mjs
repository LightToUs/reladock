import test from 'node:test';
import assert from 'node:assert/strict';

import * as authSession from '../public/auth-session.js';
import { createLatestRequestGate } from '../public/telemetry.js';

const { createLogoutGuard } = authSession;

test('logout blocks relogin and invalidates requests from the previous session until it settles', async () => {
  const authGate = createLatestRequestGate();
  const guard = createLogoutGuard(authGate);
  const oldRequest = authGate.current();
  let finishLogout;
  const logoutResponse = new Promise(resolve => { finishLogout = resolve; });

  const logoutToken = guard.start();
  const pending = logoutResponse.then(() => guard.finish(logoutToken));
  assert.equal(guard.isPending(), true);
  assert.equal(guard.canLogin(), false);
  assert.equal(authGate.isCurrent(oldRequest), false);

  finishLogout();
  assert.equal(await pending, true);
  assert.equal(guard.isPending(), false);
  assert.equal(guard.canLogin(), true);
});

test('a second logout cannot replace the pending logout token', () => {
  const guard = createLogoutGuard(createLatestRequestGate());
  const first = guard.start();
  assert.equal(guard.start(), undefined);
  assert.equal(guard.finish(first + 1), false);
  assert.equal(guard.isPending(), true);
  assert.equal(guard.finish(first), true);
});

test('password visibility toggles without changing the password and can be reset to hidden', () => {
  assert.equal(typeof authSession.bindPasswordVisibility, 'function');
  const attributes = new Map();
  const button = new EventTarget();
  button.textContent = '';
  button.setAttribute = (name, value) => attributes.set(name, String(value));
  const input = { type: 'password', value: 'secret-value' };

  const visibility = authSession.bindPasswordVisibility(input, button);
  assert.equal(input.type, 'password');
  assert.equal(button.textContent, '显示');
  assert.equal(attributes.get('aria-pressed'), 'false');

  button.dispatchEvent(new Event('click'));
  assert.equal(input.type, 'text');
  assert.equal(input.value, 'secret-value');
  assert.equal(button.textContent, '隐藏');
  assert.equal(attributes.get('aria-pressed'), 'true');

  visibility.hide();
  assert.equal(input.type, 'password');
  assert.equal(input.value, 'secret-value');
  assert.equal(button.textContent, '显示');
  assert.equal(attributes.get('aria-pressed'), 'false');
});
