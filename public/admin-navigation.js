export function bindAdminNavigation({ root, toggle, title, items, documentTarget, onSelect }) {
  const itemListeners = new Map();

  function setOpen(open) {
    if (open) root.classList.add('nav-open');
    else root.classList.remove('nav-open');
    toggle.setAttribute('aria-expanded', String(open));
  }

  function focusTitle() {
    title.setAttribute('tabindex', '-1');
    title.focus({ preventScroll: true });
    title.addEventListener('blur', () => title.removeAttribute('tabindex'), { once: true });
  }

  function close({ focus = 'none' } = {}) {
    const wasOpen = root.classList.contains('nav-open');
    setOpen(false);
    if (!wasOpen) return;
    if (focus === 'title') focusTitle();
    if (focus === 'toggle') toggle.focus();
  }

  const toggleListener = () => setOpen(!root.classList.contains('nav-open'));
  const keyListener = event => { if (event.key === 'Escape' && root.classList.contains('nav-open')) close({ focus: 'toggle' }); };
  toggle.addEventListener('click', toggleListener);
  documentTarget.addEventListener('keydown', keyListener);

  for (const item of items) {
    const listener = () => { onSelect(item.dataset.view); close({ focus: 'title' }); };
    item.addEventListener('click', listener);
    itemListeners.set(item, listener);
  }

  return {
    close,
    destroy() {
      toggle.removeEventListener('click', toggleListener);
      documentTarget.removeEventListener('keydown', keyListener);
      for (const [item, listener] of itemListeners) item.removeEventListener('click', listener);
    },
  };
}
