import test from 'node:test';
import assert from 'node:assert/strict';

let bindAdminNavigation;
try {
  ({ bindAdminNavigation } = await import('../public/admin-navigation.js'));
} catch {
  bindAdminNavigation = undefined;
}

class FakeClassList {
  constructor() { this.values = new Set(); }
  contains(name) { return this.values.has(name); }
  add(name) { this.values.add(name); }
  toggle(name) { if (this.values.has(name)) { this.values.delete(name); return false; } this.values.add(name); return true; }
  remove(name) { this.values.delete(name); }
}

class FakeElement extends EventTarget {
  constructor(view = '') { super(); this.classList = new FakeClassList(); this.dataset = { view }; this.attributes = new Map(); this.focused = 0; }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name); }
  removeAttribute(name) { this.attributes.delete(name); }
  focus() { this.focused += 1; }
}

function keydown(key) {
  const event = new Event('keydown');
  Object.defineProperty(event, 'key', { value: key });
  return event;
}

test('mobile admin navigation expands, selects dynamic items, and restores focus', () => {
  assert.equal(typeof bindAdminNavigation, 'function');
  const root = new FakeElement();
  const toggle = new FakeElement();
  const title = new FakeElement();
  const documentTarget = new EventTarget();
  const items = [new FakeElement('overview'), new FakeElement('costs'), new FakeElement('notifications')];
  const selected = [];
  const binding = bindAdminNavigation({ root, toggle, title, items, documentTarget, onSelect: view => selected.push(view) });

  toggle.dispatchEvent(new Event('click'));
  assert.equal(root.classList.contains('nav-open'), true);
  assert.equal(toggle.getAttribute('aria-expanded'), 'true');

  items[2].dispatchEvent(new Event('click'));
  assert.deepEqual(selected, ['notifications']);
  assert.equal(root.classList.contains('nav-open'), false);
  assert.equal(toggle.getAttribute('aria-expanded'), 'false');
  assert.equal(title.focused, 1);
  assert.equal(title.getAttribute('tabindex'), '-1');

  toggle.dispatchEvent(new Event('click'));
  documentTarget.dispatchEvent(keydown('Escape'));
  assert.equal(root.classList.contains('nav-open'), false);
  assert.equal(toggle.focused, 1);
  binding.destroy();
});
