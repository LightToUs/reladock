import test from 'node:test';
import assert from 'node:assert/strict';

let bindAccountMenu;
try {
  ({ bindAccountMenu } = await import('../public/account-menu.js'));
} catch {
  bindAccountMenu = undefined;
}

class FakeElement extends EventTarget {
  constructor(view = '') {
    super();
    this.dataset = { accountView: view };
    this.hidden = true;
    this.attributes = new Map();
    this.focused = 0;
    this.children = [];
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name); }
  focus() { this.focused += 1; }
  contains(target) { return target === this || this.children.includes(target); }
}

class FakeDocument {
  constructor() { this.listeners = new Map(); }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(listener);
  }
  removeEventListener(type, listener) { this.listeners.get(type)?.delete(listener); }
  emit(type, values = {}) {
    const event = { type, ...values };
    for (const listener of this.listeners.get(type) || []) listener(event);
  }
}

function fixture() {
  const root = new FakeElement();
  const trigger = new FakeElement();
  const items = [new FakeElement('team'), new FakeElement('password'), new FakeElement('operations')];
  root.children.push(...items);
  const documentTarget = new FakeDocument();
  const selected = [];
  const binding = bindAccountMenu?.({ root, trigger, items, documentTarget, onSelect: view => selected.push(view) });
  return { root, trigger, items, documentTarget, selected, binding };
}

test('account button toggles one direct-action menu', () => {
  assert.equal(typeof bindAccountMenu, 'function');
  const view = fixture();
  assert.equal(view.root.hidden, true);
  assert.equal(view.trigger.getAttribute('aria-expanded'), 'false');
  view.trigger.dispatchEvent(new Event('click'));
  assert.equal(view.root.hidden, false);
  assert.equal(view.trigger.getAttribute('aria-expanded'), 'true');
  view.trigger.dispatchEvent(new Event('click'));
  assert.equal(view.root.hidden, true);
});

test('selecting an account destination closes the menu and returns the direct route', () => {
  const view = fixture();
  view.trigger.dispatchEvent(new Event('click'));
  view.items[2].dispatchEvent(new Event('click'));
  assert.deepEqual(view.selected, ['operations']);
  assert.equal(view.root.hidden, true);
  assert.equal(view.trigger.getAttribute('aria-expanded'), 'false');
});

test('outside click closes while an inside click keeps the menu open', () => {
  const view = fixture();
  view.trigger.dispatchEvent(new Event('click'));
  view.documentTarget.emit('click', { target: view.items[0] });
  assert.equal(view.root.hidden, false);
  view.documentTarget.emit('click', { target: new FakeElement() });
  assert.equal(view.root.hidden, true);
});

test('Escape closes the menu and restores focus to the account button', () => {
  const view = fixture();
  view.trigger.dispatchEvent(new Event('click'));
  view.documentTarget.emit('keydown', { key: 'Escape' });
  assert.equal(view.root.hidden, true);
  assert.equal(view.trigger.focused, 1);
});

test('callers can close the disclosure before a slow account action', () => {
  const view = fixture();
  view.trigger.dispatchEvent(new Event('click'));
  view.binding.close();
  assert.equal(view.root.hidden, true);
  assert.equal(view.trigger.getAttribute('aria-expanded'), 'false');
});

test('destroy removes every account menu listener', () => {
  const view = fixture();
  view.binding.destroy();
  view.trigger.dispatchEvent(new Event('click'));
  view.items[0].dispatchEvent(new Event('click'));
  view.documentTarget.emit('keydown', { key: 'Escape' });
  assert.equal(view.root.hidden, true);
  assert.deepEqual(view.selected, []);
});
