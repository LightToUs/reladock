import test from 'node:test';
import assert from 'node:assert/strict';

let bindConsoleNavigation;
try {
  ({ bindConsoleNavigation } = await import('../public/console-navigation.js'));
} catch {
  bindConsoleNavigation = undefined;
}

class FakeClassList {
  constructor() { this.values = new Set(); }
  contains(name) { return this.values.has(name); }
  add(name) { this.values.add(name); }
  toggle(name, force) { const enabled = force === undefined ? !this.values.has(name) : Boolean(force); if (enabled) this.values.add(name); else this.values.delete(name); return enabled; }
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

test('admin navigation includes runtime items in URL-backed page selection', () => {
  assert.equal(typeof bindConsoleNavigation, 'function');
  const root = new FakeElement();
  const toggle = new FakeElement();
  const collapse = new FakeElement();
  const title = new FakeElement();
  const documentTarget = new EventTarget();
  const items = [new FakeElement('overview'), new FakeElement('costs'), new FakeElement('notifications')];
  const panes = items.map(item => { const pane = new FakeElement(); pane.dataset.pane = item.dataset.view; return pane; });
  const selected = [];
  const locationTarget = { hash: '#/overview', pathname: '/admin', search: '' };
  const historyTarget = { pushState(_state, _title, path) { locationTarget.hash = path.slice(path.indexOf('#')); }, replaceState(_state, _title, path) { locationTarget.hash = path.slice(path.indexOf('#')); } };
  const storage = { getItem() { return null; }, setItem() {} };
  const binding = bindConsoleNavigation({ root, sidebar: root, mobileToggle: toggle, collapseToggle: collapse, title, items, panes, documentTarget, windowTarget: documentTarget, locationTarget, historyTarget, storage, onSelect: view => selected.push(view) });

  toggle.dispatchEvent(new Event('click'));
  assert.equal(root.classList.contains('nav-open'), true);
  assert.equal(toggle.getAttribute('aria-expanded'), 'true');

  items[2].dispatchEvent(new Event('click'));
  assert.deepEqual(selected, ['overview', 'notifications']);
  assert.equal(locationTarget.hash, '#/notifications');
  assert.equal(panes[2].hidden, false);
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
