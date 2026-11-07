export function bindAccountMenu({
  root,
  trigger,
  items,
  documentTarget = globalThis.document,
  onSelect = () => {},
}) {
  const menuItems = [...items];

  function setOpen(open, { focus = false } = {}) {
    root.hidden = !open;
    trigger.setAttribute('aria-expanded', String(open));
    if (!open && focus) trigger.focus();
  }

  const triggerListener = () => setOpen(root.hidden);
  const documentClickListener = event => {
    if (root.hidden || root.contains(event.target) || trigger.contains(event.target)) return;
    setOpen(false);
  };
  const keyListener = event => {
    if (event.key === 'Escape' && !root.hidden) setOpen(false, { focus: true });
  };
  const itemListeners = new Map();

  trigger.addEventListener('click', triggerListener);
  documentTarget.addEventListener('click', documentClickListener);
  documentTarget.addEventListener('keydown', keyListener);
  for (const item of menuItems) {
    const listener = event => {
      event.preventDefault?.();
      onSelect(item.dataset.accountView);
      setOpen(false);
    };
    item.addEventListener('click', listener);
    itemListeners.set(item, listener);
  }
  setOpen(false);

  return {
    close: () => setOpen(false),
    destroy() {
      trigger.removeEventListener('click', triggerListener);
      documentTarget.removeEventListener('click', documentClickListener);
      documentTarget.removeEventListener('keydown', keyListener);
      for (const [item, listener] of itemListeners) item.removeEventListener('click', listener);
      setOpen(false);
    },
  };
}
