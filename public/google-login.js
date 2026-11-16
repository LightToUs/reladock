const genericStartError = 'Google 登录暂时不可用，请稍后重试。';
const genericCallbackError = 'Google 登录未完成，请重试或使用邮箱密码登录。';
const genericLinkMessage = '首次使用 Google 登录，请输入现有 RelaDock 账号邮箱和密码完成绑定。';

export function createGoogleLoginController({ api, returnTo, button, message, location, history, onAvailabilityChange = () => {} }) {
  if (returnTo !== '/' && returnTo !== '/admin') throw new Error('invalid Google login return target');
  let enabled = false;
  let pending = false;

  function setAvailable(value) {
    enabled = value === true;
    button.hidden = !enabled;
    onAvailabilityChange(enabled);
  }

  async function load() {
    try {
      const status = await api('/api/auth/google/status');
      setAvailable(status?.enabled === true);
    } catch {
      setAvailable(false);
    }
    return enabled;
  }

  async function start() {
    if (!enabled || pending) return false;
    pending = true;
    button.disabled = true;
    message.textContent = '';
    try {
      const result = await api('/api/auth/google/start', { method: 'POST', body: { returnTo } });
      const authorization = new URL(result?.authorizationURL || '');
      if (authorization.origin !== 'https://accounts.google.com' || authorization.username || authorization.password) throw new Error('invalid authorization URL');
      location.assign(authorization.href);
      return true;
    } catch {
      message.textContent = genericStartError;
      return false;
    } finally {
      pending = false;
      button.disabled = false;
    }
  }

  function consumeCallbackResult() {
    const current = new URL(location.href);
    const result = current.searchParams.get('googleAuth');
    if (result !== 'failed' && result !== 'link') return '';
    message.textContent = result === 'link' ? genericLinkMessage : genericCallbackError;
    current.searchParams.delete('googleAuth');
    history.replaceState(null, '', current.pathname + current.search + current.hash);
    return result;
  }

  button.addEventListener('click', event => {
    event.preventDefault();
    void start();
  });

  return { load, start, consumeCallbackResult };
}
