export function bindNodeContentDialog({ dialog, closeButton, onClose = () => {} }) {
  let active = null;
  let session = 0;

  function close() {
    if (dialog.open) dialog.close();
  }

  function open(value) {
    if (!dialog.open || active?.id !== value?.id) session++;
    active = value;
    if (!dialog.open) dialog.showModal();
  }

  closeButton.addEventListener('click', close);
  dialog.addEventListener('cancel', event => {
    event.preventDefault();
    close();
  });
  dialog.addEventListener('close', () => {
    const closed = active;
    session++;
    active = null;
    onClose(closed);
  });

  return { open, close, current: () => active, session: () => session, isCurrent: token => dialog.open && token === session };
}

export async function runDialogSession(controller, task, handlers = {}) {
  const session = controller.session();
  try {
    const value = await task();
    if (!controller.isCurrent(session)) return false;
    await handlers.success?.(value);
    return true;
  } catch (error) {
    if (!controller.isCurrent(session)) return false;
    await handlers.error?.(error);
    return false;
  } finally {
    if (controller.isCurrent(session)) await handlers.finally?.();
  }
}

export function bindNodeDeliveryDialog({ dialog, closeButton, title, status, load, render }) {
  let requestVersion = 0;
  const content = bindNodeContentDialog({
    dialog,
    closeButton,
    onClose: () => {
      requestVersion++;
      status.textContent = '';
    },
  });

  async function open(node) {
    const version = ++requestVersion;
    title.textContent = `交付记录 · ${node.name}`;
    status.textContent = '正在加载交付记录…';
    render([]);
    content.open(node);
    try {
      const records = await load(node.id);
      if (version !== requestVersion || !dialog.open || content.current()?.id !== node.id) return;
      render(records);
      status.textContent = records.length ? `最近 ${records.length} 份配置交付记录。` : '暂无交付记录。';
    } catch (error) {
      if (version !== requestVersion || !dialog.open || content.current()?.id !== node.id) return;
      status.textContent = error.message || '交付记录加载失败。';
    }
  }

  return { open, close: content.close, current: content.current };
}
