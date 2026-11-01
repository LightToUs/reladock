function routePath(locationTarget, view) {
  return `${locationTarget.pathname || ''}${locationTarget.search || ''}#/${encodeURIComponent(view)}`;
}

function routeView(locationTarget) {
  const match = /^#\/([a-z0-9-]+)$/.exec(locationTarget.hash || '');
  return match ? decodeURIComponent(match[1]) : '';
}

export function bindConsoleNavigation({
  root,
  sidebar,
  mobileToggle,
  collapseToggle,
  title,
  description,
  items,
  panes,
  defaultView = 'overview',
  titles = {},
  descriptions = {},
  storageKey = 'reladock.sidebar.collapsed',
  storage = globalThis.localStorage,
  windowTarget = globalThis.window,
  documentTarget = globalThis.document,
  locationTarget = globalThis.location,
  historyTarget = globalThis.history,
  onSelect = () => {},
}) {
  const menuItems = [...items];
  const pagePanes = [...panes];
  const views = new Set(pagePanes.map(pane => pane.dataset.pane));
  const itemListeners = new Map();
  let currentView = '';

  function focusTitle() {
    if (!title) return;
    title.setAttribute('tabindex', '-1');
    title.focus({ preventScroll: true });
    title.addEventListener('blur', () => title.removeAttribute('tabindex'), { once: true });
  }

  function setMobileOpen(open) {
    root.classList.toggle('nav-open', open);
    mobileToggle?.setAttribute('aria-expanded', String(open));
  }

  function closeMobile({ focus = 'none' } = {}) {
    const wasOpen = root.classList.contains('nav-open');
    setMobileOpen(false);
    if (!wasOpen) return;
    if (focus === 'toggle') mobileToggle?.focus();
    if (focus === 'title') focusTitle();
  }

  function apply(view, { focus = false } = {}) {
    const next = views.has(view) ? view : defaultView;
    currentView = next;
    for (const pane of pagePanes) pane.hidden = pane.dataset.pane !== next;
    for (const item of menuItems) {
      const selected = item.dataset.view === next;
      item.classList.toggle('selected', selected);
      if (selected) item.setAttribute('aria-current', 'page'); else item.removeAttribute('aria-current');
    }
    if (title) title.textContent = titles[next] || next;
    if (description) description.textContent = descriptions[next] || '';
    onSelect(next);
    if (focus) focusTitle();
    return next;
  }

  function replace(view) {
    historyTarget.replaceState(null, '', routePath(locationTarget, view));
    return apply(view);
  }

  function sync() {
    const requested = routeView(locationTarget);
    return views.has(requested) ? apply(requested) : replace(defaultView);
  }

  function select(view, { replace: shouldReplace = false, focus = false } = {}) {
    const next = views.has(view) ? view : defaultView;
    const path = routePath(locationTarget, next);
    if (routeView(locationTarget) !== next) {
      if (shouldReplace) historyTarget.replaceState(null, '', path);
      else historyTarget.pushState(null, '', path);
    }
    return apply(next, { focus });
  }

  function setCollapsed(collapsed) {
    root.classList.toggle('sidebar-collapsed', collapsed);
    sidebar?.setAttribute('aria-label', collapsed ? '已收起的功能菜单' : '功能菜单');
    collapseToggle?.setAttribute('aria-expanded', String(!collapsed));
    collapseToggle?.setAttribute('aria-label', collapsed ? '展开侧栏' : '收起侧栏');
    try { storage?.setItem(storageKey, String(collapsed)); } catch { /* local storage may be unavailable */ }
  }

  let collapsed = false;
  try { collapsed = storage?.getItem(storageKey) === 'true'; } catch { /* local storage may be unavailable */ }
  setCollapsed(collapsed);

  const mobileListener = () => setMobileOpen(!root.classList.contains('nav-open'));
  const collapseListener = () => setCollapsed(!root.classList.contains('sidebar-collapsed'));
  const historyListener = () => sync();
  const keyListener = event => {
    if (event.key === 'Escape' && root.classList.contains('nav-open')) closeMobile({ focus: 'toggle' });
  };
  mobileToggle?.addEventListener('click', mobileListener);
  collapseToggle?.addEventListener('click', collapseListener);
  windowTarget?.addEventListener('popstate', historyListener);
  windowTarget?.addEventListener('hashchange', historyListener);
  documentTarget?.addEventListener('keydown', keyListener);

  for (const item of menuItems) {
    const listener = event => {
      event.preventDefault?.();
      select(item.dataset.view);
      closeMobile({ focus: 'title' });
    };
    item.addEventListener('click', listener);
    itemListeners.set(item, listener);
  }

  setMobileOpen(false);
  sync();

  return {
    select,
    sync,
    closeMobile,
    current: () => currentView,
    destroy() {
      mobileToggle?.removeEventListener('click', mobileListener);
      collapseToggle?.removeEventListener('click', collapseListener);
      windowTarget?.removeEventListener('popstate', historyListener);
      windowTarget?.removeEventListener('hashchange', historyListener);
      documentTarget?.removeEventListener('keydown', keyListener);
      for (const [item, listener] of itemListeners) item.removeEventListener('click', listener);
    },
  };
}
