import { assessConnection, buildInquirySummary } from './scope-checker.js';

document.documentElement.classList.add('has-js');

const customerCopy = Object.freeze({
  commerce: {
    title: '访问海外公开 API',
    detail: '境内业务系统通过指定端口，连接一个已确认的海外公网域名。',
    fit: '适合：订单、库存、物流等公开接口',
  },
  saas: {
    title: '给客户一个固定入口',
    detail: 'ERP / SaaS 服务商把指定公网服务，按客户需要连接到明确的跨境入口。',
    fit: '适合：公开 ERP、回调与数据接口',
  },
  warehouse: {
    title: '连接仓储公网服务',
    detail: '海外仓系统或服务器通过固定协议与端口，连接境内已公开的业务目标。',
    fit: '适合：WMS、物流与设备公开端口',
  },
});

function setupCustomerPreview() {
  const preview = document.querySelector('[data-preview]');
  const buttons = [...document.querySelectorAll('[data-customer]')];
  if (!preview || buttons.length === 0) return;

  const title = preview.querySelector('.preview-title');
  const detail = preview.querySelector('.preview-detail');
  const fit = preview.querySelector('.preview-fit');

  for (const button of buttons) {
    button.addEventListener('click', () => {
      const copy = customerCopy[button.dataset.customer];
      if (!copy) return;

      for (const candidate of buttons) {
        const selected = candidate === button;
        candidate.classList.toggle('is-active', selected);
        candidate.setAttribute('aria-pressed', String(selected));
      }

      title.textContent = copy.title;
      detail.textContent = copy.detail;
      fit.textContent = copy.fit;
    });
  }
}

function setupScopeChecker() {
  const form = document.querySelector('[data-scope-form]');
  const result = document.querySelector('[data-scope-result]');
  if (!form || !result) return;

  const title = result.querySelector('[data-result-title]');
  const detail = result.querySelector('[data-result-detail]');
  const summary = result.querySelector('[data-result-summary]');
  const copyButton = result.querySelector('[data-copy-summary]');
  const consoleLink = result.querySelector('[data-console-link]');

  form.addEventListener('submit', (event) => {
    event.preventDefault();
    const input = Object.fromEntries(new FormData(form).entries());
    const assessment = assessConnection(input);

    title.textContent = assessment.summary;
    detail.textContent = assessment.detail;
    result.hidden = false;
    result.classList.toggle('is-rejected', !assessment.inScope);
    summary.hidden = !assessment.inScope;
    copyButton.hidden = !assessment.inScope;
    consoleLink.hidden = !assessment.inScope;
    copyButton.textContent = '复制需求摘要';

    if (assessment.inScope) {
      summary.textContent = buildInquirySummary(input);
    } else {
      summary.textContent = '';
    }

    result.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  });

  copyButton.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(summary.textContent);
      copyButton.textContent = '已复制';
    } catch {
      const selection = window.getSelection();
      const range = document.createRange();
      range.selectNodeContents(summary);
      selection.removeAllRanges();
      selection.addRange(range);
      copyButton.textContent = '已选中，请复制';
    }
  });
}

function setupRevealMotion() {
  const elements = [...document.querySelectorAll('[data-reveal]')];
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  if (reduceMotion || !('IntersectionObserver' in window)) {
    for (const element of elements) element.classList.add('is-visible');
    return;
  }

  const observer = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        entry.target.classList.add('is-visible');
        observer.unobserve(entry.target);
      }
    },
    { rootMargin: '0px 0px -8% 0px', threshold: 0.08 },
  );

  for (const element of elements) observer.observe(element);
}

function setYear() {
  const year = String(new Date().getFullYear());
  for (const element of document.querySelectorAll('[data-year]')) {
    element.textContent = year;
  }
}

setupCustomerPreview();
setupScopeChecker();
setupRevealMotion();
setYear();
