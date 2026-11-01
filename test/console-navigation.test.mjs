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
  add(...names) { for (const name of names) this.values.add(name); }
  remove(...names) { for (const name of names) this.values.delete(name); }
  contains(name) { return this.values.has(name); }
  toggle(name, force) {
    const enabled = force === undefined ? !this.values.has(name) : Boolean(force);
    if (enabled) this.values.add(name); else this.values.delete(name);
    return enabled;
  }
}

class FakeElement extends EventTarget {
  constructor({ view = '', pane = '' } = {}) {
    super(); this.dataset = { view, pane }; this.classList = new FakeClassList(); this.attributes = new Map();
    this.hidden = false; this.textContent = ''; this.focused = 0;
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name); }
  removeAttribute(name) { this.attributes.delete(name); }
  focus() { this.focused += 1; }
}

class FakeStorage {
  constructor(values = {}) { this.values = new Map(Object.entries(values)); }
  getItem(key) { return this.values.get(key) ?? null; }
  setItem(key, value) { this.values.set(key, String(value)); }
}

function fixture(hash = '#/rules', stored = {}) {
  const windowTarget = new EventTarget();
  const locationTarget = { hash, pathname: '/', search: '' };
  const historyTarget = {
    paths: [],
    pushState(_state, _title, path) { this.paths.push(path); locationTarget.hash = path.slice(path.indexOf('#')); },
    replaceState(_state, _title, path) { this.paths.push(`replace:${path}`); locationTarget.hash = path.slice(path.indexOf('#')); },
  };
  const root = new FakeElement(); const sidebar = new FakeElement(); const mobileToggle = new FakeElement();
  const collapseToggle = new FakeElement(); const title = new FakeElement(); const description = new FakeElement();
  const labels = { overview: '运行概览', rules: '转发规则', team: '企业协作' };
  const items = ['overview', 'rules', 'team'].map(view => { const item = new FakeElement({ view }); item.textContent = labels[view]; return item; });
  const panes = ['overview', 'rules', 'team'].map(pane => new FakeElement({ pane }));
  const storage = new FakeStorage(stored); const selected = [];
  const binding = bindConsoleNavigation?.({
    root, sidebar, mobileToggle, collapseToggle, title, description, items, panes,
    defaultView: 'overview', titles: { overview: '运行概览', rules: '转发规则', team: '企业协作' },
    descriptions: { rules: '规则说明' }, storageKey: 'test.collapsed', storage,
    windowTarget, locationTarget, historyTarget, documentTarget: windowTarget,
    onSelect: view => selected.push(view),
  });
  return { binding, root, sidebar, mobileToggle, collapseToggle, title, description, items, panes, storage, selected, windowTarget, locationTarget, historyTarget };
}

test('direct hash opening shows only its pane and keeps title and menu aligned', () => {
  assert.equal(typeof bindConsoleNavigation, 'function');
  const view = fixture('#/rules');
  assert.equal(view.panes[0].hidden, true);
  assert.equal(view.panes[1].hidden, false);
  assert.equal(view.panes[2].hidden, true);
  assert.equal(view.items[1].classList.contains('selected'), true);
  assert.equal(view.items[1].getAttribute('aria-current'), 'page');
  assert.equal(view.items[1].getAttribute('aria-label'), '转发规则');
  assert.equal(view.title.textContent, '转发规则');
  assert.equal(view.description.textContent, '规则说明');
  assert.deepEqual(view.selected, ['rules']);
});

test('unknown routes replace the address with overview and never expose every pane', () => {
  const view = fixture('#/missing');
  assert.equal(view.locationTarget.hash, '#/overview');
  assert.equal(view.panes[0].hidden, false);
  assert.equal(view.panes.slice(1).every(pane => pane.hidden), true);
  assert.match(view.historyTarget.paths[0], /^replace:.*#\/overview$/);
});

test('menu selection creates a history entry and history events restore the matching pane', () => {
  const view = fixture('#/overview');
  view.items[2].dispatchEvent(new Event('click'));
  assert.equal(view.locationTarget.hash, '#/team');
  assert.equal(view.panes[2].hidden, false);
  assert.match(view.historyTarget.paths.at(-1), /#\/team$/);

  view.locationTarget.hash = '#/rules';
  view.windowTarget.dispatchEvent(new Event('popstate'));
  assert.equal(view.panes[1].hidden, false);
  assert.equal(view.title.textContent, '转发规则');
});

test('mobile drawer closes after selection or Escape and restores useful focus', () => {
  const view = fixture('#/overview');
  view.mobileToggle.dispatchEvent(new Event('click'));
  assert.equal(view.root.classList.contains('nav-open'), true);
  assert.equal(view.mobileToggle.getAttribute('aria-expanded'), 'true');
  assert.equal(view.items[0].focused, 1);
  view.items[1].dispatchEvent(new Event('click'));
  assert.equal(view.root.classList.contains('nav-open'), false);
  assert.equal(view.title.focused, 1);

  view.mobileToggle.dispatchEvent(new Event('click'));
  const escape = new Event('keydown'); Object.defineProperty(escape, 'key', { value: 'Escape' });
  view.windowTarget.dispatchEvent(escape);
  assert.equal(view.mobileToggle.focused, 1);
});

test('desktop collapse is restored, persisted and exposes its expanded state', () => {
  const view = fixture('#/overview', { 'test.collapsed': 'true' });
  assert.equal(view.root.classList.contains('sidebar-collapsed'), true);
  assert.equal(view.collapseToggle.getAttribute('aria-expanded'), 'false');
  view.collapseToggle.dispatchEvent(new Event('click'));
  assert.equal(view.root.classList.contains('sidebar-collapsed'), false);
  assert.equal(view.collapseToggle.getAttribute('aria-expanded'), 'true');
  assert.equal(view.storage.getItem('test.collapsed'), 'false');
});
