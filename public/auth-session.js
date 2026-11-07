export function createLogoutGuard(authGate) {
  let pendingToken;
  return {
    start() {
      if (pendingToken !== undefined) return undefined;
      authGate.invalidate();
      pendingToken = authGate.current();
      return pendingToken;
    },
    finish(token) {
      if (pendingToken === undefined || token !== pendingToken) return false;
      pendingToken = undefined;
      return true;
    },
    isPending() {
      return pendingToken !== undefined;
    },
    canLogin() {
      return pendingToken === undefined;
    },
  };
}

export function bindPasswordVisibility(input, button) {
  let visible = false;
  const apply = next => {
    visible = next === true;
    input.type = visible ? 'text' : 'password';
    button.textContent = visible ? '隐藏' : '显示';
    button.setAttribute('aria-pressed', String(visible));
    button.setAttribute('aria-label', visible ? '隐藏密码' : '显示密码');
  };
  button.addEventListener('click', () => apply(!visible));
  apply(false);
  return { hide: () => apply(false) };
}
